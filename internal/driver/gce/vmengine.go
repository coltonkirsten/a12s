package gce

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// HomeDir is where boot.sh mounts the home disk on the VM.
const HomeDir = "/mnt/disks/a12s-home"

// Guest attributes boot.sh publishes under a12s/.
const (
	attrEngineCert = "a12s/engine-cert"
	attrBootError  = "a12s/boot-error"
)

// Engines opens Computer VMs' engines over mutual TLS: the Controller presents its client certificate from the
// Fleet CA, and pins the engine's own certificate, which the VM made and published as a guest attribute.
type Engines struct {
	API        *gceapi.API
	Fleet      string
	Controller netip.Addr
	ClientCert tls.Certificate
	Local      *engine.Engine
	// Wait bounds waiting for a booting VM's engine; zero means four minutes.
	Wait time.Duration
	// Env is added to every Computer container's environment.
	Env []string
}

func (e *Engines) Open(ctx context.Context, vm VM) (Guest, error) {
	wait := e.Wait
	if wait == 0 {
		wait = 4 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	start := time.Now()
	var last error
	for {
		g, err := e.try(ctx, vm)
		if err == nil {
			return g, nil
		}
		var fatal bootError
		if errors.As(err, &fatal) && time.Since(start) >= bootGrace {
			return nil, err
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("engine of %s not answering after %s: %w", vm.Name, wait, last)
		case <-time.After(pollEvery):
		}
	}
}

// bootGrace is how long a boot error may be the previous boot's: guest attributes outlive a stop, and boot.sh clears
// the error only once it runs.
var bootGrace = 90 * time.Second

type bootError string

func (b bootError) Error() string { return "the VM's boot script failed: " + string(b) }

func (e *Engines) try(ctx context.Context, vm VM) (Guest, error) {
	if msg, err := e.API.GuestAttribute(ctx, vm.Name, attrBootError); err == nil && msg != "" && msg != "none" {
		return nil, bootError(msg)
	}
	certPEM, err := e.API.GuestAttribute(ctx, vm.Name, attrEngineCert)
	if err != nil {
		return nil, fmt.Errorf("engine certificate: %w", err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("engine certificate: not a PEM certificate")
	}
	cli, err := engineClient(vm.Address, block.Bytes, e.ClientCert)
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		cli.Close()
		return nil, fmt.Errorf("engine: %w", err)
	}
	return &vmGuest{e: e, vm: vm, eng: engine.New(cli, e.Fleet)}, nil
}

func engineClient(addr netip.Addr, pinned []byte, cert tls.Certificate) (*client.Client, error) {
	return client.New(
		client.WithHost("tcp://"+net.JoinHostPort(addr.String(), strconv.Itoa(EnginePort))),
		client.WithScheme("https"),
		client.WithHTTPClient(&http.Client{Transport: pinnedTransport(pinned, cert)}),
	)
}

func pinnedTransport(pinned []byte, cert tls.Certificate) *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			// The engine's certificate is self-signed and pinned: chain and name checks don't apply, the exact
			// certificate the VM published does.
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 || !bytes.Equal(raw[0], pinned) {
					return errors.New("engine certificate is not the one the VM published")
				}
				return nil
			},
		},
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// vmGuest is one VM's engine, holding exactly one Computer container.
type vmGuest struct {
	e   *Engines
	vm  VM
	eng *engine.Engine
}

func (g *vmGuest) containerName() string { return "a12s-" + g.e.Fleet + "-" + g.vm.Computer }

// localImageRef names an image copied from the Controller by its ID there: the copy can get another ID in this
// engine (image stores differ), and specs name the Controller's.
func localImageRef(id string) string {
	return "a12s.local/image:" + strings.TrimPrefix(id, "sha256:")
}

func (g *vmGuest) placement() engine.Placement {
	return engine.Placement{
		Name: g.containerName(),
		// The default bridge, not a user-defined network: those resolve names through the engine's embedded DNS,
		// which runs on the host and would skip the in-VM firewall's DNS block.
		Network: "bridge",
		Drive: func(_, target string) mount.Mount {
			return mount.Mount{Type: mount.TypeBind, Source: HomeDir, Target: target}
		},
		BundleVolume:  BundleVolume(g.e.Fleet),
		ProxyURL:      "http://" + net.JoinHostPort(g.e.Controller.String(), strconv.Itoa(ProxyPort)),
		ControllerURL: "https://" + net.JoinHostPort(g.e.Controller.String(), strconv.Itoa(MTLSPort)),
		Ports: network.PortMap{
			network.MustParsePort(strconv.Itoa(driver.ComputerdPort) + "/tcp"): {{HostPort: strconv.Itoa(driver.ComputerdPort)}},
		},
		Env: g.e.Env,
	}
}

// copyImage loads an image from the Controller's engine unless this engine has it, and returns the ref it runs by.
func (g *vmGuest) copyImage(ctx context.Context, ref string) (string, error) {
	id, err := g.e.Local.EnsureImage(ctx, ref)
	if err != nil {
		return "", err
	}
	local := localImageRef(id)
	if _, err := g.eng.Client().ImageInspect(ctx, local); err == nil {
		return local, nil
	} else if !engine.IsNotFound(err) {
		return "", fmt.Errorf("inspect %s: %w", local, err)
	}
	saved, err := g.e.Local.Client().ImageSave(ctx, []string{id})
	if err != nil {
		return "", fmt.Errorf("save %s: %w", id, err)
	}
	defer saved.Close()
	res, err := g.eng.Client().ImageLoad(ctx, saved, client.ImageLoadWithQuiet(true))
	if err != nil {
		return "", fmt.Errorf("load %s: %w", id, err)
	}
	loaded, err := loadedImageID(res)
	res.Close()
	if err != nil {
		return "", fmt.Errorf("load %s: %w", id, err)
	}
	if _, err := g.eng.Client().ImageTag(ctx, client.ImageTagOptions{Source: loaded, Target: local}); err != nil {
		return "", fmt.Errorf("tag %s: %w", id, err)
	}
	return local, nil
}

// loadedImageID reads "Loaded image ID: sha256:…" from the engine's load stream.
func loadedImageID(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var id string
	for sc.Scan() {
		var m struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Error != "" {
			return "", errors.New(m.Error)
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(m.Stream), "Loaded image ID: "); ok {
			id = v
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("the engine reported no loaded image ID")
	}
	return id, nil
}

func (g *vmGuest) Materialize(ctx context.Context, spec driver.ComputerSpec) error {
	cli := g.eng.Client()
	res, err := cli.ContainerInspect(ctx, g.containerName(), client.ContainerInspectOptions{})
	switch {
	case engine.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("inspect container: %w", err)
	default:
		c := res.Container
		if c.State != nil && (c.State.Running || c.State.Restarting || c.State.Paused) {
			return fmt.Errorf("materialize generation %d: %w", spec.Generation, driver.ErrRunning)
		}
		if engine.ParseGeneration(c.Config.Labels) == spec.Generation {
			// A materialize interrupted mid-way is redone, never taken as finished.
			if done, err := g.eng.Bootstrapped(ctx, c.ID, spec); err != nil {
				return err
			} else if done {
				return nil
			}
		}
		if _, err := cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{}); err != nil {
			return fmt.Errorf("remove generation %s: %w", c.Config.Labels[engine.LabelGeneration], err)
		}
	}
	image, err := g.copyImage(ctx, spec.Image)
	if err != nil {
		return fmt.Errorf("image %s: %w", spec.Image, err)
	}
	bundle, err := g.copyImage(ctx, spec.Bundle.Image)
	if err != nil {
		return fmt.Errorf("bundle %s: %w", spec.Bundle.Image, err)
	}
	if _, err := g.eng.EnsureBundleAs(ctx, bundle, BundleVolume(g.e.Fleet), spec.Bundle.Image); err != nil {
		return err
	}
	spec.Image = image
	if _, err := g.eng.CreateComputer(ctx, spec, g.placement()); err != nil {
		return err
	}
	return nil
}

func (g *vmGuest) Container(ctx context.Context) (driver.ComputerState, bool, error) {
	res, err := g.eng.Client().ContainerInspect(ctx, g.containerName(), client.ContainerInspectOptions{})
	if engine.IsNotFound(err) {
		return driver.ComputerState{}, false, nil
	}
	if err != nil {
		return driver.ComputerState{}, false, err
	}
	return engine.ComputerState(g.vm.Computer, "bridge", res.Container), true, nil
}

func (g *vmGuest) Start(ctx context.Context) error {
	if _, err := g.eng.Client().ContainerStart(ctx, g.containerName(), client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

func (g *vmGuest) Stop(ctx context.Context) error {
	timeout := engine.StopTimeout
	if _, err := g.eng.Client().ContainerStop(ctx, g.containerName(), client.ContainerStopOptions{Timeout: &timeout}); err != nil && !engine.IsNotFound(err) {
		return fmt.Errorf("stop container: %w", err)
	}
	return nil
}

func (g *vmGuest) Exec(ctx context.Context, user string, cmd []string, stdin io.Reader) (int, string, error) {
	return g.eng.Exec(ctx, g.containerName(), g.vm.Computer, user, cmd, stdin)
}

func (g *vmGuest) Terminal(ctx context.Context, rows, cols uint) (driver.Terminal, error) {
	return g.eng.Terminal(ctx, g.containerName(), engine.AgentUser, engine.AgentHome, engine.TerminalShell, rows, cols)
}

func (g *vmGuest) Sessions(ctx context.Context) (int, error) {
	return g.eng.Sessions(ctx, g.containerName())
}

func (g *vmGuest) Events(ctx context.Context) <-chan driver.Event { return g.eng.Events(ctx) }

func (g *vmGuest) Close() error { return g.eng.Client().Close() }
