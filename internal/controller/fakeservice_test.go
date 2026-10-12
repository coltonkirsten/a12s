package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/coltonkirsten/a12s/internal/driver"
)

type fakeService struct {
	spec    driver.ServiceSpec
	running bool
	files   map[string][]byte // path -> data (tmpfs)
	owners  map[string]string // path -> "owner mode"
}

func (d *fakeDriver) svc(name string) *fakeService {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.services[name]
}

func (d *fakeDriver) ServiceImage(_ context.Context, id string) (driver.ServiceImage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	img, ok := d.svcImages[id]
	if !ok {
		return driver.ServiceImage{}, fmt.Errorf("no image %s", id)
	}
	img.ImageID = id
	return img, nil
}

func (d *fakeDriver) StageService(_ context.Context, spec driver.ServiceSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.services[spec.Name]; s != nil {
		switch {
		case s.running:
			return driver.ErrRunning
		case s.spec.Generation > spec.Generation:
			return driver.ErrStaleGeneration
		case s.spec.Generation == spec.Generation:
			return nil
		}
	}
	d.services[spec.Name] = &fakeService{spec: spec, files: map[string][]byte{}, owners: map[string]string{}}
	d.record("stage service %s %d", spec.Name, spec.Generation)
	return nil
}

func (d *fakeDriver) SetServicePower(_ context.Context, name string, power driver.Power, gen int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.services[name]
	switch {
	case s == nil || gen > s.spec.Generation:
		return driver.ErrNotStaged
	case gen < s.spec.Generation:
		return driver.ErrStaleGeneration
	}
	if s.running == (power == driver.PowerAwake) {
		return nil
	}
	s.running = power == driver.PowerAwake
	if s.running {
		s.files, s.owners = map[string][]byte{}, map[string]string{}
	}
	d.record("power service %s %s %d", name, power, gen)
	return nil
}

// killService makes a running Service exit unexpectedly.
func (d *fakeDriver) killService(name string, code int) {
	d.mu.Lock()
	s := d.services[name]
	s.running = false
	gen := s.spec.Generation
	d.mu.Unlock()
	d.events <- driver.Event{Type: driver.EventDied, Computer: name, Generation: gen, ExitCode: code, Service: true}
}

func (d *fakeDriver) RemoveService(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.services[name] != nil {
		delete(d.services, name)
		d.record("remove service %s", name)
	}
	return nil
}

func (d *fakeDriver) ServiceExec(_ context.Context, name, user string, cmd []string, _ io.Reader) (int, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.services[name]
	if s == nil || !s.running {
		return -1, "", fmt.Errorf("exec %s: not running", name)
	}
	if d.svcUnhealthy[name] {
		return 1, "not ready", nil
	}
	return 0, "ok " + user + " " + strings.Join(cmd, " "), nil
}

func (d *fakeDriver) DialService(_ context.Context, name string, port int) (net.Conn, error) {
	return nil, fmt.Errorf("fake driver: no network")
}

func (d *fakeDriver) ServiceLogs(_ context.Context, name string, _ bool, _ int) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte("hello from " + name + "\n"))), nil
}

func (d *fakeDriver) ServiceWrite(_ context.Context, name, path, owner, mode, dirMode string, data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.services[name]
	if s == nil || !s.running {
		return fmt.Errorf("write %s: not running", name)
	}
	s.files[path] = append([]byte(nil), data...)
	s.owners[path] = owner + " " + mode
	return nil
}

func (d *fakeDriver) RefreshImage(_ context.Context, ref string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.images[ref]; ok {
		return id, nil
	}
	return "", fmt.Errorf("no image %s", ref)
}

func (d *fakeDriver) StartLogin(_ context.Context, service string, spec driver.LoginSpec) (driver.Login, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.logins == nil {
		d.logins = map[string]driver.LoginSpec{}
	}
	d.logins[service] = spec
	d.record("login %s", service)
	return driver.Login{Container: "a12s-lab-" + service + "-login", User: spec.User, Read: []string{"/a12s/init/x/a12s", "login-read"}}, nil
}

func (d *fakeDriver) RemoveLogin(_ context.Context, service string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.logins, service)
	return nil
}
