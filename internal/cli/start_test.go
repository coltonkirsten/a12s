package cli

import (
	"slices"
	"testing"

	"github.com/moby/moby/api/types/network"
)

func TestControllerCreateOptions(t *testing.T) {
	n := NamesFor("lab")
	if n != (ControllerNames{Container: "a12s-lab-controller", Volume: "a12s-lab-controller", Network: "a12s-lab", Data: "a12s-lab-data"}) {
		t.Fatalf("names %+v", n)
	}
	o := ControllerCreateOptions(n, "lab", DefaultControllerImage, "tok", map[string]string{"a12s.fleet": "lab", "a12s.role": "controller"})
	hc, cfg := o.HostConfig, o.Config
	if o.Name != "a12s-lab-controller" || cfg.Image != "a12s/controller:dev" {
		t.Errorf("name %s image %s", o.Name, cfg.Image)
	}
	if !slices.Equal(cfg.Env, []string{"A12S_FLEET=lab", "A12S_API_TOKEN=tok", "A12S_DATA_DIR=/var/lib/a12s-data"}) {
		t.Errorf("env %v", cfg.Env)
	}
	if string(hc.NetworkMode) != "a12s-lab" || hc.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("network %s restart %s", hc.NetworkMode, hc.RestartPolicy.Name)
	}
	if !slices.Equal(hc.Binds, []string{"/var/run/docker.sock:/var/run/docker.sock"}) ||
		len(hc.Mounts) != 2 || hc.Mounts[0].Source != "a12s-lab-controller" || hc.Mounts[0].Target != "/var/lib/a12s" ||
		hc.Mounts[1].Source != "a12s-lab-data" || hc.Mounts[1].Target != "/var/lib/a12s-data" {
		t.Errorf("binds %v mounts %+v", hc.Binds, hc.Mounts)
	}
	b := hc.PortBindings[network.MustParsePort("7000/tcp")]
	if len(b) != 1 || b[0].HostIP.String() != "127.0.0.1" || b[0].HostPort != "0" {
		t.Errorf("port bindings %+v", hc.PortBindings)
	}
}
