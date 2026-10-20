package gce

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"time"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// Name selects this driver (A12S_DRIVER, driver.yaml).
const Name = "gce"

// Settings are what an operator chooses for a fleet on GCE. Everything else comes from the Controller VM itself.
type Settings struct {
	// Subnetwork is the fleet's subnet in the Controller's region; the Controller VM must be in it.
	Subnetwork string `json:"subnetwork" yaml:"subnetwork"`
	// Zone defaults to the Controller VM's.
	Zone           string   `json:"zone,omitempty" yaml:"zone,omitempty"`
	Families       []string `json:"families,omitempty" yaml:"families,omitempty"`
	DefaultShape   string   `json:"defaultShape,omitempty" yaml:"defaultShape,omitempty"`
	BootImage      string   `json:"bootImage,omitempty" yaml:"bootImage,omitempty"`
	BootDiskGB     int64    `json:"bootDiskGb,omitempty" yaml:"bootDiskGb,omitempty"`
	SnapshotPolicy string   `json:"snapshotPolicy,omitempty" yaml:"snapshotPolicy,omitempty"`
	// Identity "per-computer" gives each agent computer its own service account; default "none".
	Identity string `json:"identity,omitempty" yaml:"identity,omitempty"`
	// A pointer, so a driver.yaml without it keeps the Controller's driver digest and its container.
	API *APISettings `json:"api,omitempty" yaml:"api,omitempty"`
}

type APISettings struct {
	// Listen "internal" also serves the owner API on the Controller VM's internal address, over TLS, on APIPort.
	Listen string `json:"listen" yaml:"listen"`
}

const (
	APIListenInternal = "internal"
	// APIPort is fixed, unlike the loopback port, so a firewall rule can name it.
	APIPort = 7000
)

func (s Settings) Validate() error {
	if s.Subnetwork == "" {
		return errors.New("gce.subnetwork is required")
	}
	if s.API != nil && s.API.Listen != APIListenInternal {
		return fmt.Errorf("gce.api.listen %q: want %s", s.API.Listen, APIListenInternal)
	}
	return nil
}

func (s Settings) internalAPI() bool { return s.API != nil && s.API.Listen == APIListenInternal }

// Open runs on the Controller VM: the project, zone, address and service account come from its metadata server,
// API calls use that service account's token, and the local engine is the fleet's image store.
func Open(ctx context.Context, fleet string, s Settings, stateDir string, clientCert tls.Certificate, caPEM []byte) (*Driver, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("gce driver: %w", err)
	}
	host, err := controllerVM(ctx, s.Zone)
	if err != nil {
		return nil, err
	}
	api, controller, account := host.api, host.address, host.account
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("gce driver: the Controller VM's engine: %w", err)
	}
	local := engine.New(cli, fleet)
	engines := &Engines{API: api, Fleet: fleet, Controller: controller, ClientCert: clientCert, Local: local}
	if s.Identity == IdentityPerComputer {
		engines.Env = IdentityEnv()
	}
	identity := s.Identity
	if identity == "" {
		identity = IdentityNone
	}
	d, err := New(ctx, Config{
		Fleet: fleet, API: api, Subnetwork: s.Subnetwork, Controller: controller, ControllerAccount: account,
		Families: s.Families, DefaultShape: s.DefaultShape, BootImage: s.BootImage, BootDiskGB: s.BootDiskGB,
		SnapshotPolicy: s.SnapshotPolicy, StateDir: stateDir, Local: local, Guest: engines.Open,
		Identity: s.Identity, BootMetadata: BootMetadata(controller, caPEM, identity), Arch: runtime.GOARCH,
		InternalAPI: s.internalAPI(), ControllerHostname: host.hostname,
	})
	if err != nil {
		cli.Close()
		return nil, err
	}
	d.closeLocal = cli.Close
	return d, nil
}

type controllerHost struct {
	api      *gceapi.API
	address  netip.Addr
	account  string
	hostname string
}

// controllerVM reads the project, zone, address and service account from the metadata server of the VM this runs
// on, and makes an API client that uses that account's token.
func controllerVM(ctx context.Context, zone string) (controllerHost, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	get := func(path string) (string, error) {
		var v string
		err := gceapi.MetadataGet(ctx, hc, gceapi.DefaultMetadataEndpoint, path, &v)
		return v, err
	}
	var h controllerHost
	project, err := get("project/project-id")
	if err != nil {
		return h, fmt.Errorf("gce driver: not on a GCE VM? %w", err)
	}
	if zone == "" {
		z, err := get("instance/zone")
		if err != nil {
			return h, fmt.Errorf("gce driver: %w", err)
		}
		zone = last(z)
	}
	ip, err := get("instance/network-interfaces/0/ip")
	if err != nil {
		return h, fmt.Errorf("gce driver: %w", err)
	}
	if h.address, err = netip.ParseAddr(ip); err != nil {
		return h, fmt.Errorf("gce driver: the Controller's address %q: %w", ip, err)
	}
	if h.account, err = get("instance/service-accounts/default/email"); err != nil {
		return h, fmt.Errorf("gce driver: the Controller VM has no service account: %w", err)
	}
	if h.hostname, err = get("instance/hostname"); err != nil {
		return h, fmt.Errorf("gce driver: %w", err)
	}
	h.api = &gceapi.API{
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
		Compute: gceapi.DefaultComputeEndpoint,
		IAM:     gceapi.DefaultIAMEndpoint,
		CRM:     gceapi.DefaultCRMEndpoint,
		Project: project,
		Zone:    zone,
		Token:   gceapi.MetadataToken(hc, gceapi.DefaultMetadataEndpoint),
	}
	return h, nil
}

// Host: Computers reach the Controller by address (they resolve no names). The owner API is on loopback, and with
// api.listen internal also on the internal address, over TLS, for apps in the VPC.
func (d *Driver) Host() driver.Host {
	ctl := d.cfg.Controller.String()
	h := driver.Host{ControllerName: ctl, APIAddr: "127.0.0.1:7000", RemoteEngines: true}
	if d.cfg.InternalAPI {
		h.TLSAPI = driver.TLSAPI{Addr: net.JoinHostPort(ctl, strconv.Itoa(APIPort)), Names: []string{ctl}}
		if d.cfg.ControllerHostname != "" {
			h.TLSAPI.Names = append(h.TLSAPI.Names, d.cfg.ControllerHostname)
		}
	}
	return h
}
