// Package controller reconciles an applied Fleet onto a Driver and serves the CLI's API.
package controller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/computerd"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/docker"
	"github.com/coltonkirsten/a12s/internal/driver/gce"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

type Config struct {
	Fleet    string // A12S_FLEET
	StateDir string // A12S_STATE_DIR, default /var/lib/a12s
	// DataDir holds fleet.db, the Fleet's data that outlives the
	// Controller (inboxes, holds, tokens): the a12s-F-data volume. Empty
	// means StateDir.
	DataDir    string       // A12S_DATA_DIR
	APIToken   string       // A12S_API_TOKEN
	SubnetPool netip.Prefix // A12S_SUBNET_POOL, default 10.213.0.0/16
	// APIAddr overrides the API listen address (host:port). By default the
	// API listens on the Controller's IP on the bridge network a12s-F.
	APIAddr string // A12S_API_ADDR
	// TimeScale divides power-policy durations; 1 in production, larger in tests.
	TimeScale float64 // A12S_TEST_TIME_SCALE
	// Driver is the substrate: "docker" (default) or "gce", with GCE's settings.
	Driver string       // A12S_DRIVER
	GCE    gce.Settings // A12S_GCE, JSON
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Fleet:    os.Getenv("A12S_FLEET"),
		StateDir: os.Getenv("A12S_STATE_DIR"),
		APIToken: os.Getenv("A12S_API_TOKEN"),
		APIAddr:  os.Getenv("A12S_API_ADDR"),
		DataDir:  os.Getenv("A12S_DATA_DIR"),
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/a12s"
	}
	cfg.SubnetPool = docker.DefaultSubnetPool
	cfg.Driver = os.Getenv("A12S_DRIVER")
	switch cfg.Driver {
	case "", docker.Name:
		cfg.Driver = docker.Name
	case gce.Name:
		if err := json.Unmarshal([]byte(os.Getenv("A12S_GCE")), &cfg.GCE); err != nil {
			return cfg, fmt.Errorf("A12S_GCE: %w", err)
		}
	default:
		return cfg, fmt.Errorf("A12S_DRIVER: want %s or %s, got %q", docker.Name, gce.Name, cfg.Driver)
	}
	if s := os.Getenv("A12S_TEST_TIME_SCALE"); s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 1 {
			return cfg, fmt.Errorf("A12S_TEST_TIME_SCALE: want a number >= 1, got %q", s)
		}
		cfg.TimeScale = v
	}
	if s := os.Getenv("A12S_SUBNET_POOL"); s != "" {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return cfg, fmt.Errorf("A12S_SUBNET_POOL: %w", err)
		}
		cfg.SubnetPool = p
	}
	switch {
	case cfg.Fleet == "":
		return cfg, errors.New("A12S_FLEET is required")
	case len(cfg.APIToken) < 16:
		return cfg, errors.New("A12S_API_TOKEN is required (at least 16 characters)")
	}
	return cfg, nil
}

// Timing holds the Controller's waits; tests shorten them.
type Timing struct {
	ReadyTimeout   time.Duration // computerd answering after start
	Poll           time.Duration // status polling and stream reconnects
	SleepFlush     time.Duration // bounded flush of an interrupted turn
	RestartBackoff time.Duration // first restart delay after an unexpected death; doubles
	MaxRestarts    int           // then Error{retryable}
	ReadBack       time.Duration // a Driver change showing in Observe
	PowerTick      time.Duration // power-policy evaluation
	// Service health: polling during startup, and caps on the manifest's
	// startup grace and interval (zero: the manifest's).
	HealthPoll     time.Duration
	HealthStartup  time.Duration
	HealthInterval time.Duration
}

var defaultTiming = Timing{
	ReadyTimeout:   3 * time.Minute,
	Poll:           500 * time.Millisecond,
	SleepFlush:     20 * time.Second,
	RestartBackoff: time.Second,
	MaxRestarts:    3,
	ReadBack:       10 * time.Second,
	PowerTick:      time.Second,
	HealthPoll:     time.Second,
}

type computerdClient interface {
	Status(ctx context.Context) (api.ComputerdStatus, error)
	StartTurn(ctx context.Context, req api.TurnRequest) (api.TurnResponse, error)
	TurnSummary(ctx context.Context, turnID string) (api.TurnSummary, error)
	JournalInbox(ctx context.Context, records any) ([]string, error)
	Stop(ctx context.Context, turnID string) error
	StopAll(ctx context.Context) (api.StopResponse, error)
	Events(ctx context.Context, lastEventID, turn string) (*computerd.Stream, error)
	Diagnostic(ctx context.Context, message string, data any) error
	EgressDenied(ctx context.Context, d api.EgressDeniedData) error
	CancelContinuation(ctx context.Context, reason string) (string, error)
	CloseIdleConnections()
}

type Controller struct {
	cfg Config
	drv driver.Driver
	// now is the power policy's clock (time.Now; a test moves it).
	now       func() time.Time
	inbox     *inbox.Store
	inboxKick chan struct{}
	closeOnce sync.Once
	ca        *CA
	timing    Timing
	logger    *log.Logger
	ctx       context.Context // set by Start; outlives requests

	newClient func(name string) computerdClient
	// remoteEngines: a12s shell can't reach Computers' containers from the Controller's host.
	remoteEngines bool

	// bg counts the Controller's background goroutines (event streams,
	// restarts, idle applies, the watch), so Stop can wait for them.
	bg sync.WaitGroup
	// bgMu serializes admission to bg (spawn's check and Add) with Wait closing it, so no
	// worker is added once Wait has begun.
	bgMu     sync.Mutex
	stopping bool

	// applyMu is held exclusively by Fleet-wide changes (apply, resume,
	// drive rm) and shared by single-Computer ones (power, restarts, idle
	// applies), which also take that Computer's lock: one slow Computer
	// never delays another.
	applyMu sync.RWMutex

	mu    sync.Mutex // guards everything below
	state *State
	creds api.Credentials // persisted by setCredsLocked
	// credTimes is when each credential's value last changed.
	credTimes map[string]time.Time
	rt        map[string]*runtime
	locks     map[string]*sync.Mutex // per Computer, see applyMu
	clients   map[string]computerdClient
	drains    map[string]*drainState
	srt       map[string]*serviceRuntime
	// auditSubs are followers of a Service's audit lines (service logs -f).
	auditSubs map[string][]chan string
	// revoked are principals whose certificate was withdrawn (fail closed
	// even if the registry write failed); a new registration clears it.
	revoked map[string]bool
	// openTurns are Turns being folded for the history, by "<agent computer>/<turn>".
	openTurns map[string]*openTurn
	hub       *hub
	// inboxBcast is closed and replaced on every inbox change (Message streams).
	inboxBcast chan struct{}
	// inboxDirty are the agent computers whose inbox.changed is due.
	inboxDirty map[string]bool
}

func New(cfg Config, drv driver.Driver, ca *CA) (*Controller, error) {
	st, err := loadState(filepath.Join(cfg.StateDir, stateFile))
	if err != nil {
		return nil, err
	}
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = cfg.StateDir
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	store, err := inbox.Open(filepath.Join(dataDir, "fleet.db"))
	if err != nil {
		return nil, err
	}
	c := &Controller{
		inbox:     store,
		inboxKick: make(chan struct{}, 1),
		cfg:       cfg,
		drv:       drv,
		ca:        ca,
		timing:    defaultTiming,
		now:       time.Now,
		logger:    log.New(os.Stderr, "a12s controller: ", log.LstdFlags),
		ctx:       context.Background(),
		state:     st,
		creds:     api.Credentials{},
		rt:        map[string]*runtime{},
		locks:     map[string]*sync.Mutex{},
		clients:   map[string]computerdClient{},
		srt:       map[string]*serviceRuntime{},
		auditSubs: map[string][]chan string{},
		revoked:   map[string]bool{},
		openTurns: map[string]*openTurn{},
		hub:       newHub(),
	}
	if err := c.loadCreds(); err != nil {
		store.Close()
		return nil, err
	}
	store.OnChange(c.inboxChanged)
	clientTLS, err := ca.ClientTLS()
	if err != nil {
		store.Close()
		return nil, err
	}
	c.newClient = func(name string) computerdClient {
		return computerd.New(name, func(ctx context.Context) (net.Conn, error) {
			return drv.Connect(ctx, name, driver.ServiceComputerd)
		}, clientTLS)
	}
	return c, nil
}

// lockComputer takes applyMu shared, then the Computer's own lock.
func (c *Controller) lockComputer(name string) func() {
	c.applyMu.RLock()
	c.mu.Lock()
	l := c.locks[name]
	if l == nil {
		l = &sync.Mutex{}
		c.locks[name] = l
	}
	c.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		c.applyMu.RUnlock()
	}
}

// spawn runs f in a background goroutine that Wait waits for, and reports
// whether it did: nothing new starts once the Controller's context ended.
func (c *Controller) spawn(f func()) bool {
	c.bgMu.Lock()
	if c.stopping || (c.ctx != nil && c.ctx.Err() != nil) {
		c.bgMu.Unlock()
		return false
	}
	c.bg.Add(1)
	c.bgMu.Unlock()
	go func() {
		defer c.bg.Done()
		f()
	}()
	return true
}

// Wait returns once the background goroutines have ended; call it after
// the context passed to Start is done. From its first call on, spawn admits
// nothing new.
func (c *Controller) Wait() {
	c.bgMu.Lock()
	c.stopping = true
	c.bgMu.Unlock()
	c.bg.Wait()
	c.closeStore()
}

// closeStore closes fleet.db once nothing of the Controller runs anymore.
func (c *Controller) closeStore() {
	c.closeOnce.Do(func() {
		if c.inbox != nil {
			_ = c.inbox.Close()
		}
	})
}

func (c *Controller) logf(format string, args ...any) { c.logger.Printf(format, args...) }

func (c *Controller) saveLocked() error {
	return c.state.save(filepath.Join(c.cfg.StateDir, stateFile))
}

func (c *Controller) client(name string) computerdClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl := c.clients[name]
	if cl == nil {
		cl = c.newClient(name)
		c.clients[name] = cl
	}
	return cl
}

func (c *Controller) containerName(name string) string { return "a12s-" + c.cfg.Fleet + "-" + name }

// Start resumes the persisted Fleet (attaching to running Computers,
// starting those that should be awake) and watches the Driver for deaths.
// It returns at once; work continues until ctx ends.
func (c *Controller) Start(ctx context.Context) {
	c.ctx = ctx
	if n := c.timeScaleNote(); n != "" {
		c.logf("%s", n)
	}
	c.spawn(func() { c.watch(ctx) })
	c.spawn(func() { c.expiryLoop(ctx) })
	c.spawn(func() { c.powerLoop(ctx) })
	// Taken here, before Start returns, and released by the resume pass: an
	// apply or power request that arrives first waits for the resume
	// instead of the resume re-converging after it.
	c.applyMu.Lock()
	resumed := c.spawn(func() {
		defer c.applyMu.Unlock()
		c.mu.Lock()
		f := c.state.Fleet
		c.mu.Unlock()
		if f == nil {
			return
		}
		if err := c.syncInboxes(ctx, f, "controller"); err != nil {
			c.logf("resume: inboxes: %v", err)
		}
		obs, err := c.drv.Observe(ctx)
		if err != nil {
			c.logf("resume: %v", err)
			return
		}
		running := map[string]bool{}
		for n, st := range obs.Computers {
			running[KindComputer+":"+n] = st.Running
		}
		for n, st := range obs.Services {
			running[KindService+":"+n] = st.Running
		}
		c.reconcileCerts(ctx, running)
		// Finish removals an apply had not completed: owned Computers the
		// Fleet no longer declares go (their drives stay).
		for _, name := range slices.Sorted(maps.Keys(obs.Computers)) {
			if _, ok := f.Computers[name]; !ok {
				if err := c.removeComputer(ctx, name); err != nil {
					c.logf("resume: remove %s: %v", name, err)
				}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(obs.Services)) {
			if _, ok := f.Services[name]; !ok {
				if err := c.removeService(ctx, name); err != nil {
					c.logf("resume: remove service %s: %v", name, err)
				}
			}
		}
		c.convergeServices(ctx, f.ServiceNames(), obs)
		c.convergeAll(ctx, f.ComputerNames(), obs)
		c.kickAll()
	})
	if !resumed { // ctx already ended: nothing will release it
		c.applyMu.Unlock()
	}
}

func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	ca, err := LoadCA(cfg.StateDir, cfg.Fleet)
	if err != nil {
		return err
	}
	drv, host, closeDriver, err := openDriver(ctx, cfg, ca)
	if err != nil {
		return err
	}
	defer closeDriver()
	c, err := New(cfg, drv, ca)
	if err != nil {
		return err
	}
	c.remoteEngines = host.RemoteEngines

	apiAddr := cfg.APIAddr
	if apiAddr == "" {
		apiAddr = host.APIAddr
	}
	serverTLS, err := ca.ServerTLS(host.ControllerName)
	if err != nil {
		return err
	}

	apiLn, err := net.Listen("tcp", apiAddr)
	if err != nil {
		return err
	}
	mtlsLn, err := net.Listen("tcp", ":"+strconv.Itoa(MTLSPort))
	if err != nil {
		return err
	}
	proxyLn, err := net.Listen("tcp", ":"+strconv.Itoa(ProxyPort))
	if err != nil {
		return err
	}
	servers := []*http.Server{
		{Handler: c.Handler(), ReadHeaderTimeout: 10 * time.Second},
		{Handler: c.MTLSHandler(), TLSConfig: serverTLS, ReadHeaderTimeout: 10 * time.Second},
		{Handler: c.newProxy(), ReadHeaderTimeout: 10 * time.Second},
	}
	c.Start(ctx)
	errc := make(chan error, 3)
	go func() { errc <- servers[0].Serve(apiLn) }()
	go func() { errc <- servers[1].Serve(tls.NewListener(mtlsLn, serverTLS)) }()
	go func() { errc <- servers[2].Serve(proxyLn) }()
	c.logf("fleet %s: API on %s, mTLS listener on :%d, egress proxy on :%d", cfg.Fleet, apiAddr, MTLSPort, ProxyPort)

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	c.logf("shutting down")
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shut)
	}
	if ctx.Err() != nil {
		c.Wait()
	}
	c.mu.Lock()
	if serr := c.saveLocked(); serr != nil && err == nil {
		err = serr
	}
	c.mu.Unlock()
	return err
}

func (c *Controller) newProxy() *Proxy {
	p := NewProxy(c.identify, c.policy, c.denied)
	p.Internal = c.internalTarget
	return p
}

// identify logs every refusal: a refused connection is an agent computer whose egress the Driver cannot attribute.
func (c *Controller) identify(local, remote netip.Addr) (string, bool) {
	name, ok := c.drv.Caller(local, remote)
	if !ok {
		c.logf("egress proxy: refused a connection from %s on %s: no verified agent computer for those addresses", remote, local)
	}
	return name, ok
}

func (c *Controller) policy(name string) (Policy, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if srec := c.state.Services[name]; c.state.Fleet != nil && srec != nil {
		return NewPolicy(srec.Active.Allow, nil), true
	}
	rec := c.state.Computers[name]
	if c.state.Fleet == nil || rec == nil {
		return Policy{}, false
	}
	return NewPolicy(rec.Active.Allow, rec.Active.SignInHosts), true
}

func (c *Controller) egressModeLocked(rec *Record) string {
	if slices.Contains(rec.Active.Allow, fleet.AllowAll) {
		return api.EgressAllowAll
	}
	return api.EgressEnforced
}

func (c *Controller) denied(name, host string, port int, reason string) {
	if c.isService(name) {
		c.auditf(name, "egress denied %s (%s)", net.JoinHostPort(host, strconv.Itoa(port)), reason)
		return
	}
	c.logf("%s: egress denied %s (%s)", name, net.JoinHostPort(host, strconv.Itoa(port)), reason)
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	if err := c.client(name).EgressDenied(ctx, api.EgressDeniedData{Host: host, Port: port, Reason: reason}); err != nil {
		c.logf("%s: journal egress denial: %v", name, err)
	}
}

// openDriver returns the substrate the Controller runs on and how it is reached there.
func openDriver(ctx context.Context, cfg Config, ca *CA) (driver.Driver, driver.Host, func(), error) {
	if cfg.Driver == gce.Name {
		leaf, err := ca.ControllerClientCert()
		if err != nil {
			return nil, driver.Host{}, nil, err
		}
		cert, err := leaf.TLS()
		if err != nil {
			return nil, driver.Host{}, nil, err
		}
		drv, err := gce.Open(ctx, cfg.Fleet, cfg.GCE, cfg.StateDir, cert, ca.CertPEM())
		if err != nil {
			return nil, driver.Host{}, nil, err
		}
		return drv, drv.Host(), func() { drv.Close() }, nil
	}
	drv, host, err := docker.Open(ctx, docker.Config{Fleet: cfg.Fleet, SubnetPool: cfg.SubnetPool})
	if err != nil {
		return nil, driver.Host{}, nil, err
	}
	return drv, host, func() { drv.Close() }, nil
}
