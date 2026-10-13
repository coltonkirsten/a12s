package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
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
	"github.com/coltonkirsten/a12s/internal/version"
)

var DefaultControllerImage = version.Image("controller")

const (
	ControllerPort     = "7000/tcp"
	controllerStateDir = "/var/lib/a12s"
	controllerDataDir  = "/var/lib/a12s-data"
	dockerSocket       = "/var/run/docker.sock"
)

type ControllerNames struct {
	Container string // a12s-F-controller
	Volume    string // a12s-F-controller
	Network   string // a12s-F
	// Data holds fleet.db and is kept by down.
	Data string
}

func NamesFor(fleetName string) ControllerNames {
	name := "a12s-" + fleetName + "-controller"
	return ControllerNames{Container: name, Volume: name, Network: "a12s-" + fleetName, Data: "a12s-" + fleetName + "-data"}
}

type StartOptions struct {
	Fleet string
	Image string // DefaultControllerImage when empty
	// Wait of zero means 60s.
	Wait time.Duration
	Log  func(string)
}

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

	token, prevPort := "", 0
	if cf, err := ReadControllerFile(o.Fleet); err == nil {
		token, prevPort = cf.Token, cf.Port
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	res, err := dc.ContainerInspect(ctx, n.Container, client.ContainerInspectOptions{})
	switch {
	case cerrdefs.IsNotFound(err):
		token = NewToken()
		if err := createController(ctx, dc, n, o, labels, token, prevPort); err != nil {
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
		if res.Container.Config.Image != o.Image {
			// Another release's Controller: replace it. Its volumes, so the Fleet's state, stay, and the agent
			// computers keep running while it restarts.
			if _, err := dc.ContainerRemove(ctx, n.Container, client.ContainerRemoveOptions{Force: true}); err != nil {
				return nil, fmt.Errorf("replace %s: %w", n.Container, err)
			}
			if err := createController(ctx, dc, n, o, labels, token, prevPort); err != nil {
				return nil, err
			}
			logf("replaced controller %s (%s -> %s)", n.Container, res.Container.Config.Image, o.Image)
			break
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

// ensureDataVolume labels the volume a12s.data so that down keeps it like a drive.
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

// ControllerCreateOptions publishes the API port on loopback only, on a fixed host port so that a restart of the
// container keeps the port in controller.json.
func ControllerCreateOptions(n ControllerNames, fleetName, image, token string, hostPort int, labels map[string]string) client.ContainerCreateOptions {
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
				HostPort: strconv.Itoa(hostPort),
			}}},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{n.Network: {}},
		},
	}
}

func createController(ctx context.Context, dc *client.Client, n ControllerNames, o StartOptions, labels map[string]string, token string, prevPort int) error {
	port, err := freeLoopbackPort(prevPort)
	if err != nil {
		return fmt.Errorf("pick a port for %s: %w", n.Container, err)
	}
	opts := ControllerCreateOptions(n, o.Fleet, o.Image, token, port, labels)
	// Lets tests run power-policy timers faster than real time.
	if v := os.Getenv("A12S_TEST_TIME_SCALE"); v != "" {
		opts.Config.Env = append(opts.Config.Env, "A12S_TEST_TIME_SCALE="+v)
	}
	// Any command may start the Controller first (creds set before up on a new install), so each one pulls it.
	PullMissing(ctx, []string{o.Image}, os.Stderr)
	_, err = dc.ContainerCreate(ctx, opts)
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("controller image %s not found: pull it or pass --controller-image", o.Image)
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", n.Container, err)
	}
	if _, err := dc.ContainerStart(ctx, n.Container, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start %s: %w", n.Container, err)
	}
	return nil
}

// freeLoopbackPort is prev if it is free on 127.0.0.1, else a port the kernel picks.
func freeLoopbackPort(prev int) (int, error) {
	if prev > 0 && prev < 65536 {
		if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(prev))); err == nil {
			ln.Close()
			return prev, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

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
