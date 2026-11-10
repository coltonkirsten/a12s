package controller

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/credsource"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// sourceRead is what the last read of a credential's source found. It never holds the value.
type sourceRead struct {
	version string
	err     string
}

// sourceLoop keeps every credential that names a source at the source's current value: a new version reaches the
// agent computers that use it within one SourcePoll, and a withdrawn one stops them.
func (c *Controller) sourceLoop(ctx context.Context) {
	poll := c.timing.SourcePoll
	if poll <= 0 {
		poll = defaultTiming.SourcePoll
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		c.readSources(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.sourceKick:
		}
	}
}

func (c *Controller) kickSources() {
	select {
	case c.sourceKick <- struct{}{}:
	default:
	}
}

func (c *Controller) readSources(ctx context.Context) {
	c.mu.Lock()
	var sources map[string]fleet.CredentialSource
	if f := c.state.Fleet; f != nil {
		sources = maps.Clone(f.Credentials)
	}
	for name := range c.sourced {
		if _, ok := sources[name]; !ok {
			delete(c.sourced, name)
		}
	}
	c.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		if ctx.Err() != nil {
			return
		}
		c.readSource(ctx, name, sources[name])
	}
}

func (c *Controller) readSource(ctx context.Context, name string, src fleet.CredentialSource) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	secret, err := c.secrets.Resolve(rctx, src)
	cancel()
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	if f := c.state.Fleet; f == nil || f.Credentials[name] != src {
		// The Fleet changed its source while this read ran: the next read uses the new one.
		c.mu.Unlock()
		return
	}
	prev := c.sourced[name]
	next := sourceRead{version: prev.version}
	var update api.Credentials
	switch {
	case err == nil:
		next.version = secret.Version
		if !bytes.Equal(c.creds[name], secret.Value) {
			update = api.Credentials{name: secret.Value}
		}
	case errors.Is(err, credsource.ErrNoValue):
		next = sourceRead{err: err.Error()}
		if _, ok := c.creds[name]; ok {
			update = api.Credentials{name: nil}
		}
	default:
		next.err = err.Error()
	}
	c.sourced[name] = next
	var serr error
	if update != nil {
		serr = c.setCredsLocked(update)
	}
	users := c.credentialUsersLocked(name)
	c.mu.Unlock()

	switch {
	case serr != nil:
		c.logf("credential %s: store the value read from %s: %v", name, src, serr)
		return
	case update != nil && update[name] == nil:
		c.logf("credential %s: value withdrawn (%v); agent computers and Services that use it stop until %s has a value", name, err, src)
	case update != nil:
		c.logf("credential %s: value from %s", name, secret.Version)
	case next.version != prev.version:
		c.logf("credential %s: %s holds the value already in use", name, next.version)
	case next.err != "" && next.err != prev.err:
		c.logf("credential %s: %s; keeping the value in use", name, next.err)
	case next.err == "" && prev.err != "":
		c.logf("credential %s: %s readable again", name, src)
	}
	if update == nil || len(users) == 0 {
		return
	}
	// The users pick up the change as after a12s creds set: a new value is injected, a withdrawn one stops them.
	if _, err := c.applySpec(ctx, func(s *fleet.Spec) (*fleet.Spec, error) { return s, nil }, writeOpts{only: users, by: caller{principal: api.PrincipalLocal}}); err != nil {
		c.logf("credential %s: apply to %v: %v", name, users, err)
	}
}

// sourcedLocked is the source a credential's value comes from, if the Fleet names one.
func (c *Controller) sourcedLocked(name string) (fleet.CredentialSource, bool) {
	if f := c.state.Fleet; f != nil {
		src, ok := f.Credentials[name]
		return src, ok
	}
	return fleet.CredentialSource{}, false
}

func (c *Controller) sourcesLocked() map[string]fleet.CredentialSource {
	if f := c.state.Fleet; f != nil {
		return f.Credentials
	}
	return nil
}

// refuseSourcedLocked refuses values for credentials the Controller reads from a source.
func (c *Controller) refuseSourcedLocked(values api.Credentials) error {
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if src, ok := c.sourcedLocked(name); ok {
			return sourcedError(name, src)
		}
	}
	return nil
}

func sourcedError(name string, src fleet.CredentialSource) error {
	return &httpError{status: 409, body: api.Error{Code: api.ErrConflict,
		Message: "credential " + name + " comes from " + src.String() + "; the Controller reads its value from there",
		Fix:     "add a new version to " + src.GCPSecret + " (it is picked up within a minute), or remove credentials." + name + " from the Fleet to set the value by hand"}}
}
