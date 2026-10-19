package cli

import (
	"net"
	"slices"
	"testing"
)

// The Controller container's security invariants: the API is published on
// loopback only, the token reaches it by env, and both data volumes mount.
func TestControllerCreateOptions(t *testing.T) {
	n := NamesFor("lab")
	o := ControllerCreateOptions(n, "lab", DefaultControllerImage, "tok", 7123, map[string]string{"a12s.fleet": "lab", "a12s.role": "controller"}, DriverConfig{Driver: "docker"})
	hc, cfg := o.HostConfig, o.Config
	if len(hc.PortBindings) == 0 {
		t.Fatal("API port not published")
	}
	for port, bs := range hc.PortBindings {
		for _, b := range bs {
			if b.HostIP.String() != "127.0.0.1" {
				t.Errorf("%s bound on %s, want 127.0.0.1 only", port, b.HostIP)
			}
			if b.HostPort != "7123" {
				t.Errorf("%s published on host port %q, want the fixed 7123", port, b.HostPort)
			}
		}
	}
	if !slices.Contains(cfg.Env, "A12S_API_TOKEN=tok") || !slices.Contains(cfg.Env, "A12S_FLEET=lab") {
		t.Errorf("env %v", cfg.Env)
	}
	mounts := map[string]string{}
	for _, m := range hc.Mounts {
		mounts[m.Target] = m.Source
	}
	if mounts["/var/lib/a12s"] != n.Volume || mounts["/var/lib/a12s-data"] != n.Data || string(hc.NetworkMode) != n.Network {
		t.Errorf("mounts %v network %s", mounts, hc.NetworkMode)
	}
}

// A new Controller container keeps the port controller.json had while it is free, so clients keep working.
func TestFreeLoopbackPort(t *testing.T) {
	p, err := freeLoopbackPort(0)
	if err != nil || p <= 0 {
		t.Fatalf("freeLoopbackPort(0) = %d, %v", p, err)
	}
	if again, err := freeLoopbackPort(p); err != nil || again != p {
		t.Fatalf("free previous port %d: got %d, %v", p, again, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port
	if other, err := freeLoopbackPort(taken); err != nil || other == taken || other <= 0 {
		t.Fatalf("taken port %d: got %d, %v", taken, other, err)
	}
}

// On GCE the Controller shares the VM's network, so Computer VMs reach its proxy and mTLS listener; the owner API
// still listens on loopback only.
func TestControllerCreateOptionsGCE(t *testing.T) {
	n := NamesFor("lab")
	drv := DriverConfig{Driver: "gce"}
	drv.GCE.Subnetwork = "a12s-lab"
	o := ControllerCreateOptions(n, "lab", DefaultControllerImage, "tok", 7123, map[string]string{"a12s.fleet": "lab"}, drv)
	if !o.HostConfig.NetworkMode.IsHost() || len(o.HostConfig.PortBindings) != 0 {
		t.Fatalf("network %s, ports %v", o.HostConfig.NetworkMode, o.HostConfig.PortBindings)
	}
	for _, want := range []string{"A12S_API_ADDR=127.0.0.1:7123", "A12S_DRIVER=gce", `A12S_GCE={"subnetwork":"a12s-lab"}`} {
		if !slices.Contains(o.Config.Env, want) {
			t.Errorf("env lacks %s: %v", want, o.Config.Env)
		}
	}
}
