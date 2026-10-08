package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver/docker"
)

// Controller container settings (PLAN "Naming on Docker", "Controller API").
const (
	DefaultControllerImage = "a12s/controller:dev"
	ControllerPort         = "7000/tcp"
	controllerStateDir     = "/var/lib/a12s"
	controllerDataDir      = "/var/lib/a12s-data"
	dockerSocket           = "/var/run/docker.sock"
)

// ControllerNames are the Docker objects of one Fleet's Controller.
type ControllerNames struct {
	Container string // a12s-F-controller
	Volume    string // a12s-F-controller
	Network   string // a12s-F
	// Data is the Fleet's data volume (fleet.db), kept by down: a12s-F-data.
	Data string
}

// NamesFor returns the Controller's object names for a Fleet.
func NamesFor(fleetName string) ControllerNames {
	name := "a12s-" + fleetName + "-controller"
	return ControllerNames{Container: name, Volume: name, Network: "a12s-" + fleetName, Data: "a12s-" + fleetName + "-data"}
}

// StartOptions configures EnsureController.
type StartOptions struct {
	Fleet string
	Image string // DefaultControllerImage when empty
	// Wait bounds how long to wait for GET /v1/status. Zero means 60s.
	Wait time.Duration
	// Log, if set, reports what was done ("started controller …").
	Log func(string)
}

// EnsureController makes sure the Fleet's Controller container exists and
// runs, writes ~/.a12s/<fleet>/controller.json, and waits until the API
// answers. It returns a client for that API.
func EnsureController(ctx context.Context, o StartOptions) (*Client, error) {
	if o.Image == "" {
		o.Image = DefaultControllerImage
	}
	if o.Wait == 0 {
		o.Wait = 60 * time.Second
	}
	logf := func(format string, args ...any) {
		if o.Log != nil {
			o.Log(fmt.Sprintf(format, args...))
		}
	}
	dc, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	defer dc.Close()
	n := NamesFor(o.Fleet)
	labels := map[string]string{docker.LabelFleet: o.Fleet, docker.LabelRole: docker.RoleController}

	if err := ensureBridge(ctx, dc, n.Network, o.Fleet, labels); err != nil {
		return nil, err
	}
	if err := ensureStateVolume(ctx, dc, n.Volume, o.Fleet, labels); err != nil {
		return nil, err
	}
	if err := ensureDataVolume(ctx, dc, n.Data, o.Fleet); err != nil {
		return nil, err
	}

	token := ""
	if cf, err := ReadControllerFile(o.Fleet); err == nil {
		token = cf.Token
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	res, err := dc.ContainerInspect(ctx, n.Container, client.ContainerInspectOptions{})
	switch {
	case cerrdefs.IsNotFound(err):
		token = NewToken()
		if err := createController(ctx, dc, n, o, labels, token); err != nil {
			return nil, err
		}
		logf("started controller %s (%s)", n.Container, o.Image)
	case err != nil:
		return nil, fmt.Errorf("inspect %s: %w", n.Container, err)
	default:
		if res.Container.Config == nil || res.Container.Config.Labels[docker.LabelFleet] != o.Fleet {
			return nil, fmt.Errorf("container %s exists but is not the Controller of fleet %s", n.Container, o.Fleet)
		}
		// The container's own env is the source of truth for its token.
		if t := envValue(res.Container.Config.Env, "A12S_API_TOKEN"); t != "" {
			token = t
		}
		if token == "" {
			return nil, fmt.Errorf("container %s has no API token; remove it (docker rm -f %s) and run a12s up again", n.Container, n.Container)
		}
		if res.Container.State == nil || !res.Container.State.Running {
			if _, err := dc.ContainerStart(ctx, n.Container, client.ContainerStartOptions{}); err != nil {
				return nil, fmt.Errorf("start %s: %w", n.Container, err)
			}
			logf("started controller %s", n.Container)
		}
	}

	port, err := publishedPort(ctx, dc, n.Container)
	if err != nil {
		return nil, err
	}
	cf := &api.ControllerFile{Port: port, Token: token}
	if err := WriteControllerFile(o.Fleet, cf); err != nil {
		return nil, err
	}
	c := NewClient(cf)
	if err := waitReady(ctx, c, o.Wait); err != nil {
		return nil, fmt.Errorf("controller %s: %w (see docker logs %s)", n.Container, err, n.Container)
	}
	return c, nil
}

func ensureBridge(ctx context.Context, dc *client.Client, name, fleetName string, labels map[string]string) error {
	res, err := dc.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		if res.Network.Labels[docker.LabelFleet] != fleetName {
			return fmt.Errorf("network %s exists but does not belong to fleet %s", name, fleetName)
		}
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect network %s: %w", name, err)
	}
	if _, err := dc.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Labels: labels}); err != nil {
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}

func ensureStateVolume(ctx context.Context, dc *client.Client, name, fleetName string, labels map[string]string) error {
	res, err := dc.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil {
		if res.Volume.Labels[docker.LabelFleet] != fleetName {
			return fmt.Errorf("volume %s exists but does not belong to fleet %s", name, fleetName)
		}
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect volume %s: %w", name, err)
	}
	if _, err := dc.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

// ensureDataVolume creates the Fleet's data volume, labeled a12s.data so
// that down keeps it like a drive.
func ensureDataVolume(ctx context.Context, dc *client.Client, name, fleetName string) error {
	res, err := dc.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil {
		if res.Volume.Labels[docker.LabelFleet] != fleetName || res.Volume.Labels[docker.LabelData] == "" {
			return fmt.Errorf("volume %s exists but is not the data volume of fleet %s", name, fleetName)
		}
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect volume %s: %w", name, err)
	}
	labels := map[string]string{docker.LabelFleet: fleetName, docker.LabelData: "fleet.db"}
	if _, err := dc.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

// ControllerCreateOptions is the Controller container: Docker socket, state
// volume, the Fleet's bridge network, API port published on loopback only.
func ControllerCreateOptions(n ControllerNames, fleetName, image, token string, labels map[string]string) client.ContainerCreateOptions {
	port := network.MustParsePort(ControllerPort)
	return client.ContainerCreateOptions{
		Name: n.Container,
		Config: &container.Config{
			Image:        image,
			Env:          []string{"A12S_FLEET=" + fleetName, "A12S_API_TOKEN=" + token, "A12S_DATA_DIR=" + controllerDataDir},
			Labels:       labels,
			ExposedPorts: network.PortSet{port: {}},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:   container.NetworkMode(n.Network),
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Binds:         []string{dockerSocket + ":" + dockerSocket},
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: n.Volume, Target: controllerStateDir},
				{Type: mount.TypeVolume, Source: n.Data, Target: controllerDataDir},
			},
			PortBindings: network.PortMap{port: {{
				HostIP:   netip.MustParseAddr("127.0.0.1"),
				HostPort: "0",
			}}},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{n.Network: {}},
		},
	}
}

func createController(ctx context.Context, dc *client.Client, n ControllerNames, o StartOptions, labels map[string]string, token string) error {
	_, err := dc.ContainerCreate(ctx, ControllerCreateOptions(n, o.Fleet, o.Image, token, labels))
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("controller image %s not found locally: build it or pass --controller-image", o.Image)
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", n.Container, err)
	}
	if _, err := dc.ContainerStart(ctx, n.Container, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start %s: %w", n.Container, err)
	}
	return nil
}

// publishedPort reads the host port Docker picked for the API port.
func publishedPort(ctx context.Context, dc *client.Client, name string) (int, error) {
	port := network.MustParsePort(ControllerPort)
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := dc.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("inspect %s: %w", name, err)
		}
		if ns := res.Container.NetworkSettings; ns != nil {
			for _, b := range ns.Ports[port] {
				if p, err := strconv.Atoi(b.HostPort); err == nil && p > 0 {
					return p, nil
				}
			}
		}
		if res.Container.State != nil && !res.Container.State.Running && !res.Container.State.Restarting {
			return 0, fmt.Errorf("controller %s is not running (%s); see docker logs %s", name, res.Container.State.Status, name)
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("controller %s has no published port for %s", name, ControllerPort)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitReady polls GET /v1/status until it answers 200.
func waitReady(ctx context.Context, c *Client, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var last error
	for {
		req, err := c.NewRequest(ctx, http.MethodGet, "/v1/status", nil)
		if err != nil {
			return err
		}
		resp, err := c.HTTP.Do(req)
		if err == nil {
			if resp.StatusCode != http.StatusOK {
				err = ResponseError(resp)
			}
			resp.Body.Close()
			if err == nil {
				return nil
			}
			if HasCode(err, api.ErrUnauthorized) {
				return fmt.Errorf("the Controller rejected the token in controller.json: %w", err)
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready after %s: %v", wait, last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
