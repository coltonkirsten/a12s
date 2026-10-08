// Package controller is the a12s Controller (§4, §6, §7): it reconciles the
// applied Fleet onto a Driver, holds power state and credentials, issues
// certificates, injects secrets, runs the egress proxy and the lease
// listener, and serves the CLI's API.
package controller

import (
	"context"
	"crypto/tls"
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
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

// APIPort is where the CLI API listens, on the Fleet bridge only.
const APIPort = 7000

// Config is the Controller's environment.
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
	// TimeScale divides power-policy durations (idle time, the wake rate
	// window); 1 in production, larger for the exit gate's test speed.
	TimeScale float64 // A12S_TEST_TIME_SCALE
}

// ConfigFromEnv reads the Config from the environment.
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
	SleepFlush     time.Duration // §7: bounded flush of an interrupted turn
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

// computerdClient is what the Controller needs from computerd.
type computerdClient interface {
	Status(ctx context.Context) (api.ComputerdStatus, error)
	StartTurn(ctx context.Context, req api.TurnRequest) (api.TurnResponse, error)
	TurnSummary(ctx context.Context, turnID string) (api.TurnSummary, error)
	JournalInbox(ctx context.Context, records any) ([]string, error)
	Stop(ctx context.Context, turnID string) error
	Events(ctx context.Context, lastEventID, turn string) (*computerd.Stream, error)
	Diagnostic(ctx context.Context, message string, data any) error
	CloseIdleConnections()
}

// Controller reconciles one Fleet.
type Controller struct {
	cfg Config
	drv driver.Driver
	// inbox is fleet.db (enhancements §4.1).
	inbox     *inbox.Store
	inboxKick chan struct{}
	closeOnce sync.Once
	ca        *CA
	timing    Timing
	logger    *log.Logger
	ctx       context.Context // set by Start; outlives requests

	// newClient makes the computerd client of a Computer.
	newClient func(name string) computerdClient
	// selfAddrs returns the Controller's IP on each network it is attached
	// to; nil outside Docker.
	selfAddrs func(ctx context.Context) (map[string]netip.Addr, error)

	// bg counts the Controller's background goroutines (event streams,
	// restarts, idle applies, the watch), so Stop can wait for them.
	bg sync.WaitGroup
	// bgMu serializes admission to bg (spawn's check and Add) with Wait
	// closing it, so no worker is added once Wait has begun (review #60).
	bgMu     sync.Mutex
	stopping bool

	// applyMu is held exclusively by Fleet-wide changes (apply, resume,
	// drive rm) and shared by single-Computer ones (power, restarts, idle
	// applies), which also take that Computer's lock: one slow Computer
	// never delays another.
	applyMu sync.RWMutex

	// netMu serializes the proxy map's refresh (engine read and publish)
	// against removing a Computer and its network (invalidate, remove,
	// read back): a refresh can never publish an address whose network is
	// being, or has been, removed and possibly reused by another Computer.
	netMu sync.Mutex
	// netContended, if set (tests only), runs with the refresh's context
	// when a refresh finds netMu held and is about to wait for it.
	netContended func(ctx context.Context)

	mu       sync.Mutex // guards everything below
	state    *State
	creds    api.Credentials // in memory only
	rt       map[string]*runtime
	locks    map[string]*sync.Mutex // per Computer, see applyMu
	clients  map[string]computerdClient
	proxyIPs map[netip.Addr]string // Controller IP on a Computer network -> Computer
	drains   map[string]*drainState
	srt      map[string]*serviceRuntime
	// auditSubs are followers of a Service's audit lines (service logs -f).
	auditSubs map[string][]chan string
	// inboxBcast is closed and replaced on every inbox change (Message streams).
	inboxBcast chan struct{}
	addrsAt    time.Time
}

// New returns a Controller over drv, loading its state from cfg.StateDir.
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
		logger:    log.New(os.Stderr, "a12s controller: ", log.LstdFlags),
		ctx:       context.Background(),
		state:     st,
		creds:     api.Credentials{},
		rt:        map[string]*runtime{},
		locks:     map[string]*sync.Mutex{},
		clients:   map[string]computerdClient{},
		proxyIPs:  map[netip.Addr]string{},
		srt:       map[string]*serviceRuntime{},
		auditSubs: map[string][]chan string{},
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

// lockComputer takes applyMu shared and the Computer's own lock, and
// returns the unlock.
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

// ContainerName is the Computer's container on Docker (`a12s shell`).
func (c *Controller) containerName(name string) string { return "a12s-" + c.cfg.Fleet + "-" + name }

// Start resumes the persisted Fleet (attaching to running Computers,
// starting those that should be awake) and watches the Driver for deaths.
// It returns at once; work continues until ctx ends.
func (c *Controller) Start(ctx context.Context) {
	c.ctx = ctx
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
		c.refreshAddrs(ctx)
		c.kickAll()
	})
	if !resumed { // ctx already ended: nothing will release it
		c.applyMu.Unlock()
	}
}

// Run is `a12s controller`: it serves the API on the Fleet bridge, the
// lease listener and the egress proxy until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	drv, err := docker.New(docker.Config{Fleet: cfg.Fleet, SubnetPool: cfg.SubnetPool})
	if err != nil {
		return err
	}
	defer drv.Close()
	ca, err := LoadCA(cfg.StateDir, cfg.Fleet)
	if err != nil {
		return err
	}
	c, err := New(cfg, drv, ca)
	if err != nil {
		return err
	}
	c.selfAddrs = drv.ControllerAddrs

	apiAddr := cfg.APIAddr
	if apiAddr == "" {
		addrs, err := drv.ControllerAddrs(ctx)
		if err != nil {
			return err
		}
		bridge := "a12s-" + cfg.Fleet
		ip, ok := addrs[bridge]
		if !ok {
			return fmt.Errorf("the controller container is not attached to network %s", bridge)
		}
		apiAddr = net.JoinHostPort(ip.String(), strconv.Itoa(APIPort))
	}
	serverTLS, err := ca.ServerTLS(drv.ControllerName())
	if err != nil {
		return err
	}

	apiLn, err := net.Listen("tcp", apiAddr)
	if err != nil {
		return err
	}
	leaseLn, err := net.Listen("tcp", ":"+strconv.Itoa(LeasePort))
	if err != nil {
		return err
	}
	proxyLn, err := net.Listen("tcp", ":"+strconv.Itoa(ProxyPort))
	if err != nil {
		return err
	}
	servers := []*http.Server{
		{Handler: c.Handler(), ReadHeaderTimeout: 10 * time.Second},
		{Handler: NewLeases(c.authorizeLease).Handler(), TLSConfig: serverTLS, ReadHeaderTimeout: 10 * time.Second},
		{Handler: NewProxy(c.identify, c.policy, c.denied), ReadHeaderTimeout: 10 * time.Second},
	}
	c.Start(ctx)
	errc := make(chan error, 3)
	go func() { errc <- servers[0].Serve(apiLn) }()
	go func() { errc <- servers[1].Serve(tls.NewListener(leaseLn, serverTLS)) }()
	go func() { errc <- servers[2].Serve(proxyLn) }()
	c.logf("fleet %s: API on %s, leases on :%d (mTLS), egress proxy on :%d", cfg.Fleet, apiAddr, LeasePort, ProxyPort)

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

// refreshAddrs rebuilds the map from the Controller's address on each
// Computer network to that Computer.
func (c *Controller) refreshAddrs(ctx context.Context) error {
	if c.selfAddrs == nil {
		return nil
	}
	if !c.netMu.TryLock() {
		if c.netContended != nil {
			c.netContended(ctx) // tests: this refresh waits for a removal (review #59)
		}
		c.netMu.Lock()
	}
	defer c.netMu.Unlock()
	addrs, err := c.selfAddrs(ctx)
	if err != nil {
		c.logf("egress proxy: %v", err)
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addrsAt = time.Now()
	c.proxyIPs = map[netip.Addr]string{}
	if c.state.Fleet != nil {
		for name := range c.state.Fleet.Computers {
			if ip, ok := addrs[c.containerName(name)]; ok {
				c.proxyIPs[ip] = name
			}
		}
		for name := range c.state.Fleet.Services {
			if ip, ok := addrs[c.containerName(name)]; ok {
				c.proxyIPs[ip] = name
			}
		}
	}
	return nil
}

// removeNetworked removes a Computer, and so its network, and reads that
// back, holding netMu throughout after dropping its proxy mapping.
func (c *Controller) removeNetworked(ctx context.Context, name, what string) error {
	c.netMu.Lock()
	defer c.netMu.Unlock()
	c.invalidateAddrs(name)
	if err := c.drv.RemoveComputer(ctx, name); err != nil {
		return err
	}
	return c.confirm(ctx, name, what, func(_ driver.ComputerState, exists bool) bool { return !exists })
}

// invalidateAddrs forgets a Computer's address before its network is
// removed: a replacement network may reuse the subnet, and the address,
// for another Computer, which must never inherit this one's policy. The
// caller holds netMu.
func (c *Controller) invalidateAddrs(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ip, n := range c.proxyIPs {
		if n == name {
			delete(c.proxyIPs, ip)
		}
	}
}

// verifyAddr refreshes the mapping from the engine and requires the
// Computer's network to be in it, before the Computer starts serving.
func (c *Controller) verifyAddr(ctx context.Context, name string) error {
	if c.selfAddrs == nil {
		return nil
	}
	if err := c.refreshAddrs(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.proxyIPs {
		if n == name {
			return nil
		}
	}
	return fmt.Errorf("the Controller has no address on %s's network; egress would not be identifiable", name)
}

// identify maps the proxy's local address to the Computer on that network.
func (c *Controller) identify(local netip.Addr) (string, bool) {
	c.mu.Lock()
	name, ok := c.proxyIPs[local]
	stale := time.Since(c.addrsAt) > time.Second
	c.mu.Unlock()
	if ok {
		return name, true
	}
	if !stale {
		c.logf("egress proxy: refused a connection on %s: no verified Computer for that address", local)
		return "", false
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	c.refreshAddrs(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	name, ok = c.proxyIPs[local]
	if !ok {
		c.logf("egress proxy: refused a connection on %s: no verified Computer for that address", local)
	}
	return name, ok
}

// policy is a Computer's or Service's egress policy: that of its active
// generation.
func (c *Controller) policy(name string) (Policy, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if srec := c.state.Services[name]; c.state.Fleet != nil && srec != nil {
		return NewPolicy(srec.Active.Allow, nil, srec.Active.Unenforced), true
	}
	rec := c.state.Computers[name]
	if c.state.Fleet == nil || rec == nil {
		return Policy{}, false
	}
	return NewPolicy(rec.Active.Allow, rec.Active.SignInHosts, rec.Active.Unenforced), true
}

// egressMode is the active egress mode of a Computer, for status.
func (c *Controller) egressModeLocked(rec *Record) string {
	switch {
	case rec.Active.Unenforced:
		return api.EgressUnenforced
	case slices.Contains(rec.Active.Allow, fleet.AllowAll):
		return api.EgressAllowAll
	}
	return api.EgressEnforced
}

// authorizeLease allows a lease only on a shared-write drive the Computer's
// active generation attaches writable.
func (c *Controller) authorizeLease(computer, drive string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.state.Computers[computer]
	if c.state.Fleet == nil || rec == nil {
		return fmt.Errorf("%s is not a Computer of this Fleet", computer)
	}
	if d, ok := c.state.Fleet.Drives[drive]; !ok || d.Access != fleet.AccessSharedWrite {
		return fmt.Errorf("drive %s is not a shared-write drive", drive)
	}
	for _, m := range rec.Active.Drives {
		if m.Name == drive && !m.ReadOnly {
			return nil
		}
	}
	return fmt.Errorf("%s does not attach drive %s writable", computer, drive)
}

// denied journals a denied egress request in the Computer (§8 gate 7).
func (c *Controller) denied(name, host string, port int, reason string) {
	if c.isService(name) {
		c.auditf(name, "egress denied %s (%s)", net.JoinHostPort(host, strconv.Itoa(port)), reason)
		return
	}
	c.logf("%s: egress denied %s (%s)", name, net.JoinHostPort(host, strconv.Itoa(port)), reason)
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	data := map[string]any{"host": host, "port": port, "reason": reason}
	if err := c.client(name).Diagnostic(ctx, "egress denied", data); err != nil {
		c.logf("%s: journal egress denial: %v", name, err)
	}
}
