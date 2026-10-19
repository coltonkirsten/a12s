package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	Allow []string `json:"allow,omitempty"`
	// Send and Uses are what it may do; Ports what it may be reached on.
	Send  []string                     `json:"send,omitempty"`
	Uses  []driver.Endpoint            `json:"uses,omitempty"`
	Ports map[string]fleet.ServicePort `json:"ports,omitempty"`
}

type serviceRuntime struct {
	phase   api.Phase
	failure *api.Failure
	waiting *api.Waiting
	// transition is set while the Controller itself stops, stages or
	// removes the Service: deaths seen then are expected.
	transition   bool
	cancelHealth context.CancelFunc
	health       string // "ok", or the last failure
	healthSince  time.Time
	credsHash    string
	// published is the last service.phase event, so one is sent only on a change.
	published *api.ServicePhaseData
}

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
	c.setServicePhaseLocked(name, phase, failure)
}

func (c *Controller) setServicePhaseLocked(name string, phase api.Phase, failure *api.Failure) {
	c.setServiceStateLocked(name, phase, failure, nil)
}

func (c *Controller) setServiceStateLocked(name string, phase api.Phase, failure *api.Failure, waiting *api.Waiting) {
	r := c.srtLocked(name)
	if r.phase == phase && (failure == nil) == (r.failure == nil) && (failure == nil || *failure == *r.failure) && sameWaiting(waiting, r.waiting) {
		c.publishServicePhaseLocked(name)
		return
	}
	switch {
	case failure != nil:
		c.logf("service %s: %s -> Error: %s", name, r.phase, failure.Reason)
	case waiting != nil:
		c.logf("service %s: %s -> %s: %s", name, r.phase, phase, waiting)
	default:
		c.logf("service %s: %s -> %s", name, r.phase, phase)
	}
	r.phase, r.failure, r.waiting = phase, failure, waiting
	c.publishServicePhaseLocked(name)
}

// publishServicePhaseLocked sends service.phase when the Service's phase, health or restart count moved. Its
// envelope names no agent computer; the hub filters it by the Service's name.
func (c *Controller) publishServicePhaseLocked(name string) {
	r := c.srt[name]
	if r == nil || c.hub == nil {
		return
	}
	d := api.ServicePhaseData{Service: name, Phase: r.phase, Health: r.health, Error: r.failure, Waiting: r.waiting}
	if rec := c.state.Services[name]; rec != nil {
		d.Restarts = rec.Restarts
	}
	if p := r.published; p != nil && p.Phase == d.Phase && p.Health == d.Health && p.Restarts == d.Restarts &&
		(p.Error == nil) == (d.Error == nil) && (d.Error == nil || *p.Error == *d.Error) && sameWaiting(p.Waiting, d.Waiting) {
		return
	}
	r.published = &d
	data, _ := json.Marshal(d)
	ev, _ := json.Marshal(api.Event{V: api.EventVersion, TS: time.Now().UTC(), Type: api.EventServicePhase, Data: data})
	c.hub.publish(hubEvent{name: name, data: ev})
}

func (c *Controller) servicePhase(name string) api.Phase {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.srtLocked(name).phase
}

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

func (c *Controller) controllerHost() string { return "a12s-" + c.cfg.Fleet + "-controller" }

// resolveServices pins each Service's image and reads its manifest from the pinned image.
// A pin moves only with a new ref: rebuild a Service's image under a new tag.
func (c *Controller) resolveServices(ctx context.Context, f *fleet.Fleet, pins *fleet.Pins, caps driver.Capabilities) (map[string]*fleet.ServiceManifest, error) {
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
		id, err := ensurePinned(ctx, s.Image, pins.Services[key], c.drv.EnsureImage)
		if err != nil {
			return nil, fmt.Errorf("services.%s.image: %w", name, err)
		}
		pins.Services[key] = id
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

func serviceUser(m *fleet.ServiceManifest) string {
	if m == nil || m.User == nil {
		return ""
	}
	return m.User.String()
}

func (c *Controller) buildServiceSpec(name string) (driver.ServiceSpec, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, pins := c.state.Fleet, c.state.Pins
	if f == nil || pins == nil {
		return driver.ServiceSpec{}, "", errors.New("no Fleet applied")
	}
	s, ok := f.Services[name]
	if !ok {
		return driver.ServiceSpec{}, "", fmt.Errorf("no service %q in the Fleet", name)
	}
	m := c.state.Manifests[name]
	image := pins.Services[fleet.ServiceKey(name, s.Image)]
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
			"A12S_API=https://" + ctl + ":" + strconv.Itoa(MTLSPort),
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
	b, err := json.Marshal(struct {
		Image       string
		Manifest    *fleet.ServiceManifest
		Config      map[string]any
		Credentials []string
		Egress      fleet.ServiceEgress
		CPU, Memory int64
		State       fleet.Bytes
		Spec        driver.ServiceSpec
	}{image, m, s.Config, s.Credentials, s.Egress, spec.CPUMillis, spec.Memory, stateSize, spec})
	if err != nil {
		return spec, "", err
	}
	sum := sha256.Sum256(b)
	return spec, hex.EncodeToString(sum[:]), nil
}

func (c *Controller) activeServiceLocked(name string) ServiceActive {
	s := c.state.Fleet.Services[name]
	a := ServiceActive{Allow: slices.Clone(s.Egress.Allow), Send: slices.Clone(s.Grants.Send), Uses: c.endpointsLocked(s.Uses)}
	if m := c.state.Manifests[name]; m != nil {
		a.Ports = maps.Clone(m.Ports)
	}
	return a
}

func (c *Controller) observeService(ctx context.Context, name string) (driver.ComputerState, bool, error) {
	obs, err := c.drv.Observe(ctx)
	if err != nil {
		return driver.ComputerState{}, false, err
	}
	st, ok := obs.Services[name]
	return st, ok, nil
}

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

// convergeService restarts a changed Service at once: a Service has no idle boundary.
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
		// decides its phase (from Starting, which health may leave).
		c.setServicePhase(name, api.PhaseStarting, nil)
		c.watchHealth(name)
	}
	return nil
}

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

func (c *Controller) stopService(ctx context.Context, name string, gen int64) error {
	c.unregisterCert(KindService, name)
	if err := c.drv.SetServicePower(ctx, name, driver.PowerAsleep, gen); err != nil {
		return err
	}
	return c.confirmService(ctx, name, "power asleep", func(st driver.ComputerState, exists bool) bool {
		return !exists || st.Power == driver.PowerAsleep
	})
}

// startService powers on the staged generation, injects its credentials
// and config, and waits for its health check: Starting -> Ready, or
// Unhealthy once the startup grace has passed.
func (c *Controller) startService(ctx context.Context, name string) error {
	c.mu.Lock()
	s := c.state.Fleet.Services[name]
	gen := c.state.Services[name].Generation
	var w *api.Waiting
	if missing := missingCredentials(s.Credentials, c.creds); len(missing) > 0 {
		w = credentialWait(missing)
		c.setServiceStateLocked(name, api.PhaseProvisioning, nil, w)
	}
	c.mu.Unlock()
	if w != nil {
		return &credentialsMissing{w}
	}
	c.setServicePhase(name, api.PhaseStarting, nil)
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
		return &credentialsMissing{credentialWait(missing)}
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
		res.Body.Close()
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
	if changed {
		c.mu.Lock()
		c.publishServicePhaseLocked(name)
		c.mu.Unlock()
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
		_ = c.setCredsLocked(req.Credentials)
		c.state.Services[name].Restarts = 0
		_ = c.saveLocked()
		c.publishServicePhaseLocked(name)
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
		var cm *credentialsMissing
		if errors.As(err, &cm) {
			return &httpError{status: http.StatusConflict, body: api.Error{Code: api.ErrInvalid, Message: name + ": " + err.Error(), Fix: cm.waiting.Fix}}
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
	c.unregisterCert(KindService, name)
	err := c.drv.RemoveService(ctx, name)
	if err == nil {
		err = c.confirmService(ctx, name, "removal", func(_ driver.ComputerState, exists bool) bool { return !exists })
	}
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

func (c *Controller) unexpectedService(ev driver.Event) bool {
	c.mu.Lock()
	rec := c.state.Services[ev.Computer]
	r := c.srt[ev.Computer]
	ok := rec != nil && r != nil && !r.transition && ev.Generation == rec.Generation
	c.mu.Unlock()
	return ok && c.serviceDesired(c.ctx, ev.Computer) == driver.PowerAwake
}

// serviceDied restarts through the full start path, with backoff.
func (c *Controller) serviceDied(ev driver.Event) {
	if !c.unexpectedService(ev) {
		return
	}
	name := ev.Computer
	defer c.lockComputer(name)()
	if !c.unexpectedService(ev) {
		return
	}
	ctx := c.ctx
	if obs, ok, err := c.observeService(ctx, name); err != nil || !ok || obs.Running || obs.Generation != ev.Generation {
		return // alive again (a delayed event), gone, or replaced: leave it be
	}
	c.stopHealth(name)
	c.unregisterCert(KindService, name)
	c.mu.Lock()
	rec := c.state.Services[name]
	rec.Restarts++
	n := rec.Restarts
	_ = c.saveLocked()
	c.setServicePhaseLocked(name, api.PhaseStarting, nil)
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

func (c *Controller) serviceStatus(name string) api.ServiceStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.state.Fleet
	s := f.Services[name]
	out := api.ServiceStatus{Name: name, Image: s.Image, Send: s.Grants.Send, Uses: fleet.UseNames(s.Uses)}
	out.Spec, _ = json.Marshal(s)
	if c.state.Pins != nil {
		out.Pin = c.state.Pins.Services[fleet.ServiceKey(name, s.Image)]
	}
	if rec := c.state.Services[name]; rec != nil {
		out.Generation, out.Restarts = rec.Generation, rec.Restarts
		out.EgressMode = api.EgressEnforced
		if slices.Contains(rec.Active.Allow, fleet.AllowAll) {
			out.EgressMode = api.EgressAllowAll
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
	out.Phase, out.Error, out.Waiting, out.Health = r.phase, r.failure, r.waiting, r.health
	if !r.healthSince.IsZero() {
		out.HealthSince = r.healthSince.UTC().Format(time.RFC3339)
	}
	if out.EgressMode == api.EgressAllowAll {
		out.Warnings = append(out.Warnings, `egress allows any public host ("*")`)
	}
	if out.Phase == api.PhaseUnhealthy && r.health != "" {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s is Unhealthy since %s (%s)  fix: a12s service logs %s", name, r.healthSince.UTC().Format("15:04"), r.health, name))
	}
	return out
}

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

func (c *Controller) isService(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Fleet != nil && c.state.Fleet.HasService(name)
}

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
