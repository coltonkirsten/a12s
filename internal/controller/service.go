package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// ServiceRecord is one Service's persistent state (state.json).
type ServiceRecord struct {
	// Generation is the generation staged on the substrate; SpecHash the
	// hash of its spec.
	Generation int64  `json:"generation"`
	SpecHash   string `json:"specHash,omitempty"`
	// Restarts counts restarts after the Service exited, since the last
	// operator action.
	Restarts int `json:"restarts,omitempty"`
	// CredentialGeneration is the last value written to .generation.
	CredentialGeneration int64 `json:"credentialGeneration,omitempty"`
	// Active is what the Controller enforces for the staged generation.
	Active ServiceActive `json:"active"`
}

// ServiceActive is the part of a Service generation enforced outside the
// substrate: its egress policy.
type ServiceActive struct {
	Allow      []string `json:"allow,omitempty"`
	Unenforced bool     `json:"unenforced,omitempty"`
	// Send, Uses and TCP are what it may do; Ports what it may be reached
	// on; AllowPrivate the Fleet's egress.allowPrivate for its TCP egress.
	Send         []string                     `json:"send,omitempty"`
	Uses         []driver.Endpoint            `json:"uses,omitempty"`
	TCP          []fleet.TCPEgress            `json:"tcp,omitempty"`
	Ports        map[string]fleet.ServicePort `json:"ports,omitempty"`
	AllowPrivate []string                     `json:"allowPrivate,omitempty"`
}

// serviceRuntime is a Service's in-memory state.
type serviceRuntime struct {
	phase   api.Phase
	failure *api.Failure
	// transition is set while the Controller itself stops, stages or
	// removes the Service: deaths seen then are expected.
	transition   bool
	cancelHealth context.CancelFunc
	health       string // "ok", or the last failure
	healthSince  time.Time
	credsHash    string
}

// serviceWaiting is a Service left in Error for want of a credential (a
// login, or credentials not loaded): the rest of the Fleet is fine, so
// `a12s up` reports it as a warning, not a failure (§5.5).
type serviceWaiting struct{ err error }

func (e *serviceWaiting) Error() string { return e.err.Error() }
func (e *serviceWaiting) Unwrap() error { return e.err }

// MaxServiceBackoff caps the delay between restarts of a Service that keeps
// exiting.
const MaxServiceBackoff = 5 * time.Minute

func (c *Controller) srtLocked(name string) *serviceRuntime {
	r := c.srt[name]
	if r == nil {
		r = &serviceRuntime{phase: api.PhaseProvisioning}
		c.srt[name] = r
	}
	return r
}

func (c *Controller) setServicePhase(name string, phase api.Phase, failure *api.Failure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.srtLocked(name)
	if r.phase == phase && (failure == nil) == (r.failure == nil) && (failure == nil || *failure == *r.failure) {
		return
	}
	if failure != nil {
		c.logf("service %s: %s -> Error: %s", name, r.phase, failure.Reason)
	} else {
		c.logf("service %s: %s -> %s", name, r.phase, phase)
	}
	r.phase, r.failure = phase, failure
}

func (c *Controller) servicePhase(name string) api.Phase {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.srtLocked(name).phase
}

// failService puts the Service in Error with a reason (and audits it).
func (c *Controller) failService(name string, err error) error {
	c.setServicePhase(name, api.PhaseError, &api.Failure{Reason: err.Error(), Retryable: true})
	c.auditf(name, "error: %v", err)
	return err
}

func (c *Controller) setServiceTransition(name string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.srtLocked(name).transition = on
}

// auditf records a Controller audit line about a Service: in fleet.db (for
// `a12s service logs`) and the Controller's log.
func (c *Controller) auditf(name, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	c.logf("service %s: %s", name, line)
	if c.inbox == nil {
		return
	}
	l, err := c.inbox.Audit(context.WithoutCancel(c.ctx), name, line)
	if err != nil {
		c.logf("service %s: audit: %v", name, err)
		return
	}
	c.mu.Lock()
	subs := slices.Clone(c.auditSubs[name])
	c.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- l.Line:
		default:
		}
	}
}

// serviceDesired is a Service's desired power from fleet.db (awake when
// nothing is recorded).
func (c *Controller) serviceDesired(ctx context.Context, name string) driver.Power {
	if c.inbox == nil {
		return driver.PowerAwake
	}
	d, ok, err := c.inbox.ServiceDesired(ctx, name)
	if err != nil || !ok {
		return driver.PowerAwake
	}
	return driver.Power(d)
}

// controllerHost is the Controller's name on every network.
func (c *Controller) controllerHost() string { return "a12s-" + c.cfg.Fleet + "-controller" }

// resolveServices pins each Service's image, reads its manifest from the
// pinned image, merges the inline manifest and checks the result
// (§5.6). refresh names Services whose ref is resolved afresh.
func (c *Controller) resolveServices(ctx context.Context, f *fleet.Fleet, lock *fleet.Lock, caps driver.Capabilities, refresh []string) (map[string]*fleet.ServiceManifest, error) {
	out := map[string]*fleet.ServiceManifest{}
	if len(f.Services) == 0 {
		return out, nil
	}
	if !caps.Services {
		return nil, errorf(http.StatusBadRequest, api.ErrInvalid, "services: this substrate does not run Services yet (it cannot encrypt the hop to a Service across nodes)  fix: run Services on the Docker driver, or remove services:")
	}
	for _, name := range f.ServiceNames() {
		s := f.Services[name]
		key := fleet.ServiceKey(name, s.Image)
		var id string
		var err error
		if slices.Contains(refresh, name) {
			id, err = c.drv.RefreshImage(ctx, s.Image)
			if err == nil {
				lock.Services[key] = id // an explicit move: `a12s lock update services/<svc>`
			}
		} else {
			id, err = ensurePinned(ctx, s.Image, lock.Services[key], c.drv.EnsureImage)
		}
		if err != nil {
			return nil, fmt.Errorf("services.%s.image: %w", name, err)
		}
		lock.PinService(name, s.Image, id)
		img, err := c.drv.ServiceImage(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("services.%s.image: %w", name, err)
		}
		var fromImage *fleet.ServiceManifest
		if img.Manifest != nil {
			if fromImage, err = fleet.ParseManifest(img.Manifest); err != nil {
				return nil, errorf(http.StatusBadRequest, api.ErrInvalid, "services.%s: %s in %s: %v  fix: fix the image's manifest, or supply services.%s.manifest", name, fleet.ManifestPath, s.Image, err, name)
			}
		}
		m := fleet.MergeManifest(fromImage, s.Manifest)
		if err := fleet.ValidateService(name, s, m, s.Image, img.Volumes); err != nil {
			return nil, errorf(http.StatusBadRequest, api.ErrInvalid, "%v", err)
		}
		if !img.HasCommand {
			return nil, errorf(http.StatusBadRequest, api.ErrInvalid, "services.%s: image %s has no entrypoint or command  fix: use an image that runs something", name, s.Image)
		}
		out[name] = m
	}
	return out, nil
}

// serviceCredentialErrors refuses a Service credential with no material
// unless the Service's own login produces it (then only that Service waits
// in Error, §5.5).
func serviceCredentialErrors(f *fleet.Fleet, manifests map[string]*fleet.ServiceManifest, creds api.Credentials) error {
	var errs []error
	for _, name := range f.ServiceNames() {
		m := manifests[name]
		for _, cred := range missingCredentials(f.Services[name].Credentials, creds) {
			if m != nil && m.Login != nil && m.Login.Produces == cred {
				continue
			}
			errs = append(errs, fmt.Errorf("services.%s: credential %s has no binding  fix: a12s creds set %s (or add it to a12s.bindings.yaml)", name, cred, cred))
		}
	}
	if len(errs) > 0 {
		return errorf(http.StatusBadRequest, api.ErrInvalid, "%v", errors.Join(errs...))
	}
	return nil
}

// serviceUser is the manifest's uid:gid.
func serviceUser(m *fleet.ServiceManifest) string {
	if m == nil || m.User == nil {
		return ""
	}
	return m.User.String()
}

// buildServiceSpec is the Driver spec of a Service under the applied Fleet,
// Lock and manifest (generation unset), and the hash of what makes a
// Service generation.
func (c *Controller) buildServiceSpec(name string) (driver.ServiceSpec, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, lock := c.state.Fleet, c.state.Lock
	if f == nil || lock == nil {
		return driver.ServiceSpec{}, "", errors.New("no Fleet applied")
	}
	s, ok := f.Services[name]
	if !ok {
		return driver.ServiceSpec{}, "", fmt.Errorf("no service %q in the Fleet", name)
	}
	m := c.state.Manifests[name]
	image := lock.Services[fleet.ServiceKey(name, s.Image)]
	if image == "" || m == nil {
		return driver.ServiceSpec{}, "", errors.New("image is not pinned or its manifest not read; run a12s up")
	}
	ctl := c.controllerHost()
	proxy := "http://" + ctl + ":" + strconv.Itoa(ProxyPort)
	noProxy := "localhost,127.0.0.1," + ctl
	spec := driver.ServiceSpec{
		Name:  name,
		Image: image,
		User:  serviceUser(m),
		Tmpfs: slices.Clone(m.Tmpfs),
		Env: []string{
			"A12S_SERVICE=" + name,
			"A12S_FLEET=" + c.cfg.Fleet,
			"A12S_API=https://" + ctl + ":" + strconv.Itoa(LeasePort),
			"A12S_CREDENTIALS=" + strings.Join(s.Credentials, ","),
			"A12S_PROXY=" + proxy,
			"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy,
			"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
		},
	}
	if len(s.Grants.Send) > 0 {
		spec.Env = append(spec.Env, "A12S_SEND="+strings.Join(s.Grants.Send, ","))
	}
	if uses := c.endpointsLocked(s.Uses); len(uses) > 0 {
		b, _ := json.Marshal(uses)
		spec.Env = append(spec.Env, "A12S_USES="+string(b))
	}
	if tcp := tcpEgressOf(s); len(tcp) > 0 {
		b, _ := json.Marshal(tcp)
		spec.Env = append(spec.Env, "A12S_EGRESS_TCP="+string(b))
	}
	if m.Resources != nil {
		spec.CPUMillis, spec.Memory = m.Resources.CPU.Millis(), int64(m.Resources.Memory)
	}
	if s.Resources.CPU != 0 {
		spec.CPUMillis = s.Resources.CPU.Millis()
	}
	if s.Resources.Memory != 0 {
		spec.Memory = int64(s.Resources.Memory)
	}
	stateSize := fleet.Bytes(0)
	if m.State != nil {
		spec.StateDrive, spec.StateAt = driver.StateDriveID(name), m.State.At
		stateSize = m.State.Size
		if s.State.Size != 0 {
			stateSize = s.State.Size
		}
	}
	for _, p := range s.Publish {
		if pub, err := fleet.ParsePublish(p); err == nil {
			spec.Publish = append(spec.Publish, driver.PublishSpec{IP: pub.IP.String(), HostPort: pub.HostPort, Port: pub.Port})
		}
	}
	b, err := json.Marshal(struct {
		Image       string
		Manifest    *fleet.ServiceManifest
		Config      map[string]any
		Credentials []string
		Egress      fleet.ServiceEgress
		CPU, Memory int64
		State       fleet.Bytes
		Spec        driver.ServiceSpec
		Unenforced  bool `json:",omitempty"`
	}{image, m, s.Config, s.Credentials, s.Egress, spec.CPUMillis, spec.Memory, stateSize, spec, f.Egress.Unenforced()})
	if err != nil {
		return spec, "", err
	}
	sum := sha256.Sum256(b)
	return spec, hex.EncodeToString(sum[:]), nil
}

func (c *Controller) activeServiceLocked(name string) ServiceActive {
	s := c.state.Fleet.Services[name]
	a := ServiceActive{
		Allow: slices.Clone(s.Egress.Allow), Unenforced: c.state.Fleet.Egress.Unenforced(),
		Send: slices.Clone(s.Grants.Send), Uses: c.endpointsLocked(s.Uses), TCP: slices.Clone(s.Egress.TCP),
		AllowPrivate: slices.Clone(c.state.Fleet.Egress.AllowPrivate),
	}
	if m := c.state.Manifests[name]; m != nil {
		a.Ports = maps.Clone(m.Ports)
	}
	return a
}

// observeService returns the Service as the Driver sees it now.
func (c *Controller) observeService(ctx context.Context, name string) (driver.ComputerState, bool, error) {
	obs, err := c.drv.Observe(ctx)
	if err != nil {
		return driver.ComputerState{}, false, err
	}
	st, ok := obs.Services[name]
	return st, ok, nil
}

// confirmService reads the Service back until want holds.
func (c *Controller) confirmService(ctx context.Context, name, what string, want func(st driver.ComputerState, exists bool) bool) error {
	deadline := time.Now().Add(c.timing.ReadBack)
	for {
		st, exists, err := c.observeService(ctx, name)
		if err == nil && want(st, exists) {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("observed generation %d, power %s, exists %v", st.Generation, st.Power, exists)
			}
			return fmt.Errorf("service %s: %s not observed: %w", name, what, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.timing.Poll):
		}
	}
}

// convergeServices converges Services in parallel. applyMu must be held
// exclusively.
func (c *Controller) convergeServices(ctx context.Context, names []string, obs driver.Observation) map[string]outcome {
	var mu sync.Mutex
	out := map[string]outcome{}
	var wg sync.WaitGroup
	for _, name := range names {
		st, ok := obs.Services[name]
		wg.Go(func() {
			r, err := c.convergeService(ctx, name, st, ok)
			mu.Lock()
			out[name] = outcome{r, err}
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// convergeService brings one Service to the applied Fleet: an unchanged one
// only gets its power enforced; a changed one is restarted at once on a new
// generation (a Service has no idle boundary, §5.2).
func (c *Controller) convergeService(ctx context.Context, name string, obs driver.ComputerState, exists bool) (result, error) {
	spec, hash, err := c.buildServiceSpec(name)
	if err != nil {
		return unchanged, c.failService(name, err)
	}
	if spec.StateDrive != "" {
		c.mu.Lock()
		size := c.serviceStateSizeLocked(name)
		c.mu.Unlock()
		if _, err := c.drv.EnsureDrive(ctx, driver.DriveSpec{ID: spec.StateDrive, Kind: driver.DriveStateKind, Computer: name, Size: int64(size)}); err != nil {
			return unchanged, c.failService(name, err)
		}
	}
	desired := c.serviceDesired(ctx, name)
	running := exists && obs.Power == driver.PowerAwake
	c.mu.Lock()
	rec := c.state.Services[name]
	same := exists && obs.Generation == rec.Generation && hash == rec.SpecHash
	if same {
		rec.Active = c.activeServiceLocked(name)
		_ = c.saveLocked()
	}
	c.mu.Unlock()
	if same {
		return unchanged, c.ensureServicePower(ctx, name, running, desired)
	}
	return restaged, c.restageService(ctx, name, spec, hash, obs, exists, desired)
}

func (c *Controller) serviceStateSizeLocked(name string) fleet.Bytes {
	s, m := c.state.Fleet.Services[name], c.state.Manifests[name]
	size := fleet.DefaultStateSize
	if m != nil && m.State != nil && m.State.Size != 0 {
		size = m.State.Size
	}
	if s.State.Size != 0 {
		size = s.State.Size
	}
	return size
}

// ensureServicePower enforces the desired power of an up-to-date Service.
func (c *Controller) ensureServicePower(ctx context.Context, name string, running bool, desired driver.Power) error {
	c.mu.Lock()
	r := c.srtLocked(name)
	phase := r.phase
	material := c.credsHashLocked(c.state.Fleet.Services[name].Credentials)
	rotate := running && material != "" && r.credsHash != material && len(c.state.Fleet.Services[name].Credentials) > 0
	c.mu.Unlock()
	switch {
	case desired == driver.PowerAsleep && running:
		return c.sleepService(ctx, name)
	case desired == driver.PowerAsleep:
		c.setServicePhase(name, api.PhaseAsleep, nil)
		return nil
	case !running:
		return c.startService(ctx, name)
	}
	if rotate {
		if err := c.injectService(ctx, name, false); err != nil {
			return c.failService(name, err)
		}
		c.auditf(name, "credentials re-injected")
	}
	if phase != api.PhaseReady && phase != api.PhaseUnhealthy {
		// Picked up after a Controller restart: it keeps running; health
		// decides its phase.
		c.watchHealth(name)
	}
	return nil
}

// restageService stops the Service if it runs, stages a new generation and
// starts it unless it should sleep.
func (c *Controller) restageService(ctx context.Context, name string, spec driver.ServiceSpec, hash string, obs driver.ComputerState, exists bool, desired driver.Power) error {
	c.setServiceTransition(name, true)
	defer c.setServiceTransition(name, false)
	c.mu.Lock()
	rec := c.state.Services[name]
	spec.Generation = max(rec.Generation, obs.Generation) + 1
	c.mu.Unlock()
	spec.Env = append(spec.Env, "A12S_GENERATION="+strconv.FormatInt(spec.Generation, 10))
	if exists && obs.Power == driver.PowerAwake {
		c.setServicePhase(name, api.PhaseStopping, nil)
		c.stopHealth(name)
		if err := c.stopService(ctx, name, obs.Generation); err != nil {
			return c.failService(name, err)
		}
	}
	c.setServicePhase(name, api.PhaseProvisioning, nil)
	if err := c.drv.StageService(ctx, spec); err != nil {
		return c.failService(name, err)
	}
	err := c.confirmService(ctx, name, fmt.Sprintf("generation %d staged", spec.Generation), func(st driver.ComputerState, exists bool) bool {
		return exists && st.Generation == spec.Generation && !st.Running
	})
	if err != nil {
		return c.failService(name, err)
	}
	c.mu.Lock()
	rec.Generation, rec.SpecHash = spec.Generation, hash
	rec.Active = c.activeServiceLocked(name)
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.auditf(name, "generation %d staged (image %s)", spec.Generation, driver.ShortImageID(spec.Image))
	c.setServiceTransition(name, false)
	if desired == driver.PowerAwake {
		return c.startService(ctx, name)
	}
	c.setServicePhase(name, api.PhaseAsleep, nil)
	return nil
}

// stopService powers a Service down and reads that back.
func (c *Controller) stopService(ctx context.Context, name string, gen int64) error {
	c.unregisterCert(KindService, name)
	if err := c.drv.SetServicePower(ctx, name, driver.PowerAsleep, gen); err != nil {
		return err
	}
	return c.confirmService(ctx, name, "power asleep", func(st driver.ComputerState, exists bool) bool {
		return !exists || st.Power == driver.PowerAsleep
	})
}

// serviceNeedsLogin explains a missing credential: one the Service's own
// login produces is "needs a login", anything else "not loaded".
func (c *Controller) serviceCredentialError(name string, missing []string) error {
	c.mu.Lock()
	m := c.state.Manifests[name]
	loaded := len(c.creds) > 0
	c.mu.Unlock()
	if m != nil && m.Login != nil && slices.Contains(missing, m.Login.Produces) {
		return fmt.Errorf("%s needs a login  fix: a12s service login %s", name, name)
	}
	if !loaded {
		return fmt.Errorf("credentials not loaded (the Controller restarted)  fix: a12s up")
	}
	return fmt.Errorf("no material for credentials %s  fix: a12s up (check the bindings file)", strings.Join(missing, ", "))
}

// startService powers on the staged generation, injects its credentials
// and config, and waits for its health check: Starting -> Ready, or
// Unhealthy once the startup grace has passed.
func (c *Controller) startService(ctx context.Context, name string) error {
	c.setServicePhase(name, api.PhaseStarting, nil)
	c.mu.Lock()
	s := c.state.Fleet.Services[name]
	gen := c.state.Services[name].Generation
	missing := missingCredentials(s.Credentials, c.creds)
	c.mu.Unlock()
	if len(missing) > 0 {
		return &serviceWaiting{c.failService(name, c.serviceCredentialError(name, missing))}
	}
	if err := c.verifyAddr(ctx, name); err != nil {
		return c.failService(name, err)
	}
	if err := c.drv.SetServicePower(ctx, name, driver.PowerAwake, gen); err != nil {
		return c.failService(name, err)
	}
	if err := c.injectService(ctx, name, true); err != nil {
		return c.failService(name, err)
	}
	if err := c.confirmService(ctx, name, fmt.Sprintf("generation %d running", gen), func(st driver.ComputerState, exists bool) bool {
		return exists && st.Generation == gen && st.Running
	}); err != nil {
		return c.failService(name, err)
	}
	c.auditf(name, "started generation %d", gen)
	c.waitHealthy(ctx, name)
	c.watchHealth(name)
	return nil
}

// injectService writes the Service's credentials, config, at a start
// (withCert) a fresh registered certificate, and .generation last into its
// tmpfs through a12s-init (no shell needed in the image).
func (c *Controller) injectService(ctx context.Context, name string, withCert bool) error {
	c.mu.Lock()
	s, ok := c.state.Fleet.Services[name]
	rec := c.state.Services[name]
	user := serviceUser(c.state.Manifests[name])
	material := map[string][]byte{}
	for _, cred := range s.Credentials {
		material[cred] = c.creds[cred]
	}
	var err error
	if ok && rec != nil {
		rec.CredentialGeneration++
		err = c.saveLocked()
	}
	var gen, generation int64
	var send []string
	if rec != nil {
		gen, generation, send = rec.CredentialGeneration, rec.Generation, slices.Clone(rec.Active.Send)
	}
	hash := c.credsHashLocked(s.Credentials)
	c.mu.Unlock()
	switch {
	case !ok || rec == nil:
		return fmt.Errorf("inject %s: not in the Fleet", name)
	case err != nil:
		return err
	}
	if missing := missingCredentials(s.Credentials, material); len(missing) > 0 {
		return c.serviceCredentialError(name, missing)
	}
	for _, cred := range s.Credentials {
		if err := c.drv.ServiceWrite(ctx, name, secretsDir+"/"+cred, user, "0400", secretsDirMode, material[cred]); err != nil {
			return err
		}
	}
	cfg := s.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("inject %s: config: %w", name, err)
	}
	if err := c.drv.ServiceWrite(ctx, name, "/run/a12s/config/config.json", "0:0", "0644", "0755", append(data, '\n')); err != nil {
		return err
	}
	if withCert {
		leaf, err := c.ca.ServiceCert(name, generation)
		if err != nil {
			return fmt.Errorf("inject %s: %w", name, err)
		}
		if err := c.registerCert(ctx, Principal{Kind: KindService, Name: name, Generation: generation}, leaf.Serial, send); err != nil {
			return fmt.Errorf("inject %s: register certificate: %w", name, err)
		}
		for _, f := range []struct {
			name string
			data []byte
		}{{"ca.crt", c.ca.CertPEM()}, {"cert.pem", leaf.CertPEM}, {"key.pem", leaf.KeyPEM}} {
			if err := c.drv.ServiceWrite(ctx, name, tlsDir+"/"+f.name, user, "0400", "0711", f.data); err != nil {
				return err
			}
		}
	}
	if err := c.drv.ServiceWrite(ctx, name, generationFile, "0:0", "0644", secretsDirMode, []byte(strconv.FormatInt(gen, 10)+"\n")); err != nil {
		return err
	}
	c.mu.Lock()
	c.srtLocked(name).credsHash = hash
	c.mu.Unlock()
	return nil
}

// checkHealth runs the manifest's health check once.
func (c *Controller) checkHealth(ctx context.Context, name string) error {
	c.mu.Lock()
	m := c.state.Manifests[name]
	c.mu.Unlock()
	if m == nil || m.Health == nil {
		return errors.New("no health check")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if h := m.Health.HTTP; h != nil {
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return c.drv.DialService(ctx, name, h.Port)
			},
			DisableKeepAlives: true,
		}
		defer tr.CloseIdleConnections()
		p := h.Path
		if p == "" {
			p = "/"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+name+".a12s:"+strconv.Itoa(h.Port)+p, nil)
		if err != nil {
			return err
		}
		res, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			return fmt.Errorf("health: %v", unwrapNet(err))
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		res.Body.Close()
		if reauth(body) {
			return fmt.Errorf("credential: reauth  fix: a12s service login %s", name)
		}
		if res.StatusCode >= 400 {
			return fmt.Errorf("health: HTTP %d", res.StatusCode)
		}
		return nil
	}
	code, out, err := c.drv.ServiceExec(ctx, name, serviceUser(m), m.Health.Exec, nil)
	if err != nil {
		return fmt.Errorf("health: %v", err)
	}
	if code != 0 {
		out = strings.TrimSpace(out)
		if len(out) > 200 {
			out = out[:200] + "…"
		}
		return fmt.Errorf("health: exit %d: %s", code, out)
	}
	return nil
}

// reauth reports whether a health answer says the Service's session is gone
// ({"credential": "reauth"}, §5.5): it needs `a12s service login`.
func reauth(body []byte) bool {
	var h struct {
		Credential string `json:"credential"`
	}
	return json.Unmarshal(bytes.TrimSpace(body), &h) == nil && h.Credential == "reauth"
}

func unwrapNet(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err
	}
	return err
}

// recordHealth sets Ready or Unhealthy from one check (unless the Service
// is being changed) and audits a change.
func (c *Controller) recordHealth(name string, err error) {
	c.mu.Lock()
	r := c.srtLocked(name)
	if r.transition || (r.phase != api.PhaseStarting && r.phase != api.PhaseReady && r.phase != api.PhaseUnhealthy) {
		c.mu.Unlock()
		return
	}
	result := "ok"
	if err != nil {
		result = err.Error()
	}
	changed := r.health != result
	if changed {
		r.health, r.healthSince = result, time.Now()
	}
	prev := r.phase
	c.mu.Unlock()
	switch {
	case err == nil && prev != api.PhaseReady:
		c.setServicePhase(name, api.PhaseReady, nil)
		if prev == api.PhaseUnhealthy {
			c.auditf(name, "healthy again")
		} else {
			c.auditf(name, "Ready (health check passed)")
		}
	case err != nil && (prev == api.PhaseReady || prev == api.PhaseStarting):
		c.setServicePhase(name, api.PhaseUnhealthy, nil)
		c.auditf(name, "Unhealthy: %v", err)
	case err != nil && changed && prev == api.PhaseUnhealthy:
		c.auditf(name, "still unhealthy: %v", err)
	}
}

// waitHealthy polls the health check until it passes or the startup grace
// ends (then the Service is Unhealthy: reported, not restarted).
func (c *Controller) waitHealthy(ctx context.Context, name string) {
	c.mu.Lock()
	m := c.state.Manifests[name]
	c.mu.Unlock()
	startup := fleet.DefaultHealthStartup
	if m != nil && m.Health != nil {
		startup = m.Health.StartupDuration()
	}
	if c.timing.HealthStartup > 0 {
		startup = min(startup, c.timing.HealthStartup)
	}
	deadline := time.Now().Add(startup)
	for {
		err := c.checkHealth(ctx, name)
		if err == nil {
			c.recordHealth(name, nil)
			return
		}
		if st, ok, oerr := c.observeService(ctx, name); oerr == nil && (!ok || !st.Running) {
			return // it exited: the watch restarts it
		}
		if time.Now().After(deadline) {
			c.recordHealth(name, fmt.Errorf("%v (no pass within the %s startup grace)", err, startup))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.timing.HealthPoll):
		}
	}
}

// watchHealth runs the health check every interval while the Service runs.
func (c *Controller) watchHealth(name string) {
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	r := c.srtLocked(name)
	if r.cancelHealth != nil {
		r.cancelHealth()
	}
	r.cancelHealth = cancel
	m := c.state.Manifests[name]
	c.mu.Unlock()
	interval := fleet.DefaultHealthInterval
	if m != nil && m.Health != nil {
		interval = m.Health.IntervalDuration()
	}
	if c.timing.HealthInterval > 0 {
		interval = min(interval, c.timing.HealthInterval)
	}
	c.spawn(func() {
		first := true
		for ctx.Err() == nil {
			if !first {
				select {
				case <-ctx.Done():
					return
				case <-time.After(interval):
				}
			}
			first = false
			if st, ok, err := c.observeService(ctx, name); err == nil && (!ok || !st.Running) {
				return
			}
			c.recordHealth(name, c.checkHealth(ctx, name))
		}
	})
}

func (c *Controller) stopHealth(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.srt[name]; r != nil && r.cancelHealth != nil {
		r.cancelHealth()
		r.cancelHealth = nil
	}
}

// sleepService stops a Service and keeps its state drive.
func (c *Controller) sleepService(ctx context.Context, name string) error {
	if c.inbox != nil {
		if err := c.inbox.SetServiceDesired(ctx, name, string(driver.PowerAsleep)); err != nil {
			return err
		}
	}
	obs, ok, err := c.observeService(ctx, name)
	if err != nil {
		return err
	}
	c.stopHealth(name)
	if !ok || obs.Power != driver.PowerAwake {
		c.setServicePhase(name, api.PhaseAsleep, nil)
		return nil
	}
	c.setServiceTransition(name, true)
	defer c.setServiceTransition(name, false)
	c.setServicePhase(name, api.PhaseStopping, nil)
	if err := c.stopService(ctx, name, obs.Generation); err != nil {
		return c.failService(name, err)
	}
	c.setServicePhase(name, api.PhaseAsleep, nil)
	c.auditf(name, "asleep (state drive kept)")
	return nil
}

// SetServicePower is `a12s sleep <svc>` and `a12s wake <svc>`.
func (c *Controller) SetServicePower(ctx context.Context, name string, req api.ServicePowerRequest) error {
	defer c.lockComputer(name)()
	c.mu.Lock()
	inFleet := c.state.Fleet != nil && c.state.Services[name] != nil
	if inFleet {
		_, inFleet = c.state.Fleet.Services[name]
	}
	if inFleet && req.State == api.PowerAwake {
		maps.Copy(c.creds, req.Credentials)
		c.state.Services[name].Restarts = 0
		_ = c.saveLocked()
	}
	c.mu.Unlock()
	if !inFleet {
		return errorf(http.StatusNotFound, api.ErrNotFound, "no service %q in the Fleet", name)
	}
	switch req.State {
	case api.PowerAsleep:
		return c.sleepService(ctx, name)
	case api.PowerAwake:
		if c.inbox != nil {
			if err := c.inbox.SetServiceDesired(ctx, name, string(driver.PowerAwake)); err != nil {
				return err
			}
		}
		obs, ok, err := c.observeService(ctx, name)
		if err != nil {
			return err
		}
		_, err = c.convergeService(ctx, name, obs, ok)
		var waiting *serviceWaiting
		if errors.As(err, &waiting) {
			return withFix(http.StatusConflict, api.ErrInvalid, waiting.Error())
		}
		return err
	}
	return errorf(http.StatusBadRequest, api.ErrInvalid, "state must be %q or %q", api.PowerAwake, api.PowerAsleep)
}

// removeService stops and deletes a Service the Fleet no longer declares.
// Its state drive stays (an orphan until `a12s drive rm`).
func (c *Controller) removeService(ctx context.Context, name string) error {
	c.setServiceTransition(name, true)
	c.stopHealth(name)
	c.netMu.Lock()
	c.invalidateAddrs(name)
	c.unregisterCert(KindService, name)
	err := c.drv.RemoveService(ctx, name)
	if err == nil {
		err = c.confirmService(ctx, name, "removal", func(_ driver.ComputerState, exists bool) bool { return !exists })
	}
	c.netMu.Unlock()
	if err != nil {
		c.setServiceTransition(name, false)
		return err
	}
	c.mu.Lock()
	delete(c.state.Services, name)
	delete(c.state.Manifests, name)
	delete(c.srt, name)
	err = c.saveLocked()
	c.mu.Unlock()
	if c.inbox != nil {
		_ = c.inbox.ForgetService(ctx, name)
	}
	c.auditf(name, "removed from the Fleet (state drive kept)")
	return err
}

// unexpectedService reports whether a death concerns the Service's current
// generation while it should be awake and nothing is changing it.
func (c *Controller) unexpectedService(ev driver.Event) bool {
	c.mu.Lock()
	rec := c.state.Services[ev.Computer]
	r := c.srt[ev.Computer]
	ok := rec != nil && r != nil && !r.transition && ev.Generation == rec.Generation
	c.mu.Unlock()
	return ok && c.serviceDesired(c.ctx, ev.Computer) == driver.PowerAwake
}

// serviceDied restarts a Service that exited, with backoff, through the
// full start path (§5.5 "Restarts are the Controller's").
func (c *Controller) serviceDied(ev driver.Event) {
	if !c.unexpectedService(ev) {
		return
	}
	name := ev.Computer
	c.stopHealth(name)
	c.unregisterCert(KindService, name)
	defer c.lockComputer(name)()
	if !c.unexpectedService(ev) {
		return
	}
	ctx := c.ctx
	if obs, ok, err := c.observeService(ctx, name); err != nil || !ok || obs.Running || obs.Generation != ev.Generation {
		return
	}
	c.setServicePhase(name, api.PhaseStarting, nil)
	c.mu.Lock()
	rec := c.state.Services[name]
	rec.Restarts++
	n := rec.Restarts
	_ = c.saveLocked()
	c.mu.Unlock()
	backoff := min(c.timing.RestartBackoff<<min(n-1, 16), MaxServiceBackoff)
	c.auditf(name, "exited (code %d); restart %d in %s", ev.ExitCode, n, backoff)
	select {
	case <-ctx.Done():
		return
	case <-time.After(backoff):
	}
	_ = c.startService(ctx, name)
}

// serviceStatus is one Service's status row.
func (c *Controller) serviceStatus(name string) api.ServiceStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.state.Fleet
	s := f.Services[name]
	out := api.ServiceStatus{Name: name, Image: s.Image, Send: s.Grants.Send, Uses: fleet.UseNames(s.Uses), Publish: s.Publish}
	if c.state.Lock != nil {
		out.Pin = c.state.Lock.Services[fleet.ServiceKey(name, s.Image)]
	}
	if rec := c.state.Services[name]; rec != nil {
		out.Generation, out.Restarts = rec.Generation, rec.Restarts
		switch {
		case rec.Active.Unenforced:
			out.EgressMode = api.EgressUnenforced
		case slices.Contains(rec.Active.Allow, fleet.AllowAll):
			out.EgressMode = api.EgressAllowAll
		default:
			out.EgressMode = api.EgressEnforced
		}
	}
	for _, other := range f.ComputerNames() {
		if slices.Contains(fleet.UseNames(f.Computers[other].Uses), name) {
			out.UsedBy = append(out.UsedBy, other)
		}
	}
	for _, other := range f.ServiceNames() {
		if slices.Contains(fleet.UseNames(f.Services[other].Uses), name) {
			out.UsedBy = append(out.UsedBy, other)
		}
	}
	if m := c.state.Manifests[name]; m != nil {
		for _, pn := range slices.Sorted(maps.Keys(m.Ports)) {
			p := m.Ports[pn]
			out.Ports = append(out.Ports, fmt.Sprintf("%s:%d/%s", pn, p.Port, p.Proto()))
		}
		if m.State != nil {
			out.StateDrive = driver.StateDriveName(name)
		}
	}
	r := c.srtLocked(name)
	out.Phase, out.Error, out.Health = r.phase, r.failure, r.health
	if !r.healthSince.IsZero() {
		out.HealthSince = r.healthSince.UTC().Format(time.RFC3339)
	}
	for _, p := range s.Publish {
		if pub, err := fleet.ParsePublish(p); err == nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("published: %s:%d (reachable outside uses)", pub.IP, pub.HostPort))
		}
	}
	if out.EgressMode == api.EgressAllowAll {
		out.Warnings = append(out.Warnings, `egress allows any public host ("*")`)
	}
	if out.Phase == api.PhaseUnhealthy && r.health != "" {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s is Unhealthy since %s (%s)  fix: a12s service logs %s", name, r.healthSince.UTC().Format("15:04"), r.health, name))
	}
	return out
}

// Services is GET /v1/services.
func (c *Controller) Services() []api.ServiceStatus {
	c.mu.Lock()
	f := c.state.Fleet
	c.mu.Unlock()
	out := []api.ServiceStatus{}
	if f == nil {
		return out
	}
	for _, name := range f.ServiceNames() {
		out = append(out, c.serviceStatus(name))
	}
	return out
}

// isService reports whether name is a Service of the applied Fleet.
func (c *Controller) isService(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Fleet != nil && c.state.Fleet.HasService(name)
}

// writeServiceLogs writes a Service's audit lines and container output to w
// (text), following both when follow is set.
func (c *Controller) writeServiceLogs(ctx context.Context, name string, follow bool, tail int, w func(string) error) error {
	var sub chan string
	if follow {
		sub = make(chan string, 64)
		c.mu.Lock()
		c.auditSubs[name] = append(c.auditSubs[name], sub)
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.auditSubs[name] = slices.DeleteFunc(c.auditSubs[name], func(x chan string) bool { return x == sub })
			c.mu.Unlock()
		}()
	}
	if c.inbox != nil {
		lines, err := c.inbox.AuditLines(ctx, name, 0, tail)
		if err != nil {
			return err
		}
		for _, l := range lines {
			if err := w(fmt.Sprintf("[a12s %s] %s\n", l.At.Format(time.RFC3339), l.Line)); err != nil {
				return err
			}
		}
	}
	rc, err := c.drv.ServiceLogs(ctx, name, follow, tail)
	if err != nil {
		if !follow {
			return w(fmt.Sprintf("[a12s] no container output: %v\n", err))
		}
		_ = w(fmt.Sprintf("[a12s] no container output: %v\n", err))
	}
	var mu sync.Mutex
	write := func(s string) error {
		mu.Lock()
		defer mu.Unlock()
		return w(s)
	}
	done := make(chan error, 1)
	if rc != nil {
		go func() {
			defer rc.Close()
			buf := make([]byte, 32<<10)
			for {
				n, err := rc.Read(buf)
				if n > 0 {
					if werr := write(string(bytes.Clone(buf[:n]))); werr != nil {
						done <- werr
						return
					}
				}
				if err != nil {
					done <- nil
					return
				}
			}
		}()
	} else {
		done <- nil
	}
	if !follow {
		return <-done
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case line := <-sub:
			if err := write(fmt.Sprintf("[a12s %s] %s\n", time.Now().UTC().Format(time.RFC3339), line)); err != nil {
				return err
			}
		case err := <-done:
			if err != nil {
				return err
			}
			done = nil // container output ended (it stopped); audit lines continue
		}
	}
}

// withFix makes an API error from a message carrying "  fix: …".
func withFix(status int, code api.ErrorCode, msg string) error {
	m, fix, _ := strings.Cut(msg, "  fix: ")
	return &httpError{status: status, body: api.Error{Code: code, Message: m, Fix: fix}}
}
