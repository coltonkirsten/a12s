package controller

import (
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// StartLogin is POST /v1/services/{s}/login: a memory-only container of the
// pinned Service image, on the Service's network under its egress policy,
// for the CLI to run the manifest's login command in (enhancements §5.5).
// The credential never touches the Controller: the CLI reads it over the
// exec stream and writes it on the host.
func (c *Controller) StartLogin(ctx context.Context, name string) (api.LoginResponse, error) {
	c.mu.Lock()
	f := c.state.Fleet
	var s fleet.Service
	ok := f != nil && f.HasService(name)
	if ok {
		s = f.Services[name]
	}
	m := c.state.Manifests[name]
	image := ""
	if ok && c.state.Lock != nil {
		image = c.state.Lock.Services[fleet.ServiceKey(name, s.Image)]
	}
	c.mu.Unlock()
	if !ok {
		return api.LoginResponse{}, errorf(http.StatusNotFound, api.ErrNotFound, "no service %q in the Fleet", name)
	}
	if m == nil || m.Login == nil {
		return api.LoginResponse{}, &httpError{status: http.StatusBadRequest, body: api.Error{Code: api.ErrInvalid,
			Message: name + "'s manifest declares no login", Fix: "add login {command, produces} to the manifest, or set the credential with a12s creds set"}}
	}
	img, err := c.drv.ServiceImage(ctx, image)
	if err != nil {
		return api.LoginResponse{}, err
	}
	ctl := c.controllerHost()
	proxy := "http://" + ctl + ":" + strconv.Itoa(ProxyPort)
	spec := driver.LoginSpec{
		Image: image, User: serviceUser(m), Volumes: img.Volumes, Tmpfs: slices.Clone(m.Tmpfs),
		Env: []string{"A12S_SERVICE=" + name, "A12S_FLEET=" + c.cfg.Fleet, "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy, "NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1"},
	}
	if m.State != nil && !slices.Contains(spec.Volumes, m.State.At) {
		spec.Tmpfs = append(spec.Tmpfs, m.State.At) // no state drive: its path is scratch here
	}
	c.refreshAddrs(ctx) // the proxy must know the network before the command dials out
	l, err := c.drv.StartLogin(ctx, name, spec)
	if err != nil {
		return api.LoginResponse{}, err
	}
	c.refreshAddrs(ctx)
	c.auditf(name, "login started (memory-only container %s, produces %s)", l.Container, m.Login.Produces)
	return api.LoginResponse{Container: l.Container, User: l.User, Command: slices.Clone(m.Login.Command), Read: l.Read, Produces: m.Login.Produces}, nil
}

// EndLogin removes the login container, and every tmpfs with it.
func (c *Controller) EndLogin(ctx context.Context, name string) error {
	if err := c.drv.RemoveLogin(ctx, name); err != nil {
		return err
	}
	c.auditf(name, "login container removed")
	return nil
}

func (c *Controller) handleLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("s")
	res, err, ok := detached(c, r, func(ctx context.Context) (api.LoginResponse, error) { return c.StartLogin(ctx, name) })
	if !ok {
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (c *Controller) handleLoginEnd(w http.ResponseWriter, r *http.Request) {
	if err := c.EndLogin(r.Context(), r.PathValue("s")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
