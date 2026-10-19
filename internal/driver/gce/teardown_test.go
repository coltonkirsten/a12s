package gce

import (
	"slices"
	"testing"
)

func TestTeardown(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Identity = IdentityPerComputer })
	h.stageNew("scout")
	h.stageNew("tester")
	td := &Teardown{api: h.d.api, fleet: "lab", identity: true}
	p, err := td.Plan(h.ctx)
	h.must(err)
	if !slices.Equal(p.VMs, []string{"scout", "tester"}) || len(p.Disks) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	removed, destroyed, err := td.Down(h.ctx, p, false)
	h.must(err)
	if !slices.Equal(removed, []string{"scout", "tester"}) || len(destroyed) != 0 {
		t.Fatalf("down = %v, %v", removed, destroyed)
	}
	if _, ok := h.fake.Instance(InstanceName("lab", "scout")); ok || len(h.fake.ServiceAccounts()) != 0 {
		t.Fatal("a VM or service account survived down")
	}
	if _, ok := h.fake.Disk(DiskName("lab", "home-scout")); !ok {
		t.Fatal("down without purge destroyed a drive")
	}
	p, err = td.Plan(h.ctx)
	h.must(err)
	_, destroyed, err = td.Down(h.ctx, p, true)
	h.must(err)
	if len(destroyed) != 2 {
		t.Fatalf("purge destroyed %v", destroyed)
	}
}
