package cli

import (
	"slices"
	"testing"
)

// The Controller container's security invariants: the API is published on
// loopback only, the token reaches it by env, and both data volumes mount.
func TestControllerCreateOptions(t *testing.T) {
	n := NamesFor("lab")
	o := ControllerCreateOptions(n, "lab", DefaultControllerImage, "tok", map[string]string{"a12s.fleet": "lab", "a12s.role": "controller"})
	hc, cfg := o.HostConfig, o.Config
	if len(hc.PortBindings) == 0 {
		t.Fatal("API port not published")
	}
	for port, bs := range hc.PortBindings {
		for _, b := range bs {
			if b.HostIP.String() != "127.0.0.1" {
				t.Errorf("%s bound on %s, want 127.0.0.1 only", port, b.HostIP)
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
