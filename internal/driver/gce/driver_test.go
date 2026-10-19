package gce

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// One Computer through its life: the first stage boots the VM to build the container and stops it; a later stage
// only records the spec and resizes the stopped VM; wake builds the new generation; removal keeps the home disk.
func TestComputerLifecycle(t *testing.T) {
	h := newHarness(t)
	vmName := InstanceName("lab", "scout")
	spec := h.stageNew("scout")
	if calls := h.fake.Calls(); !slices.Equal(calls, []string{"disks.insert a12s-lab-home-scout", "instances.insert " + vmName, "instances.stop " + vmName}) {
		t.Fatalf("first stage calls = %v", calls)
	}
	if v := h.guests.get("scout"); !v.exists || v.built != 1 || v.running {
		t.Fatalf("first stage left the container %+v", v)
	}
	in, _ := h.fake.Instance(vmName)
	if in.Status != "TERMINATED" || last(in.MachineType) != "e2-small" || in.Scheduling.AutomaticRestart == nil || *in.Scheduling.AutomaticRestart ||
		len(in.NetworkInterfaces) != 1 || !slices.Contains(in.Tags.Items, NetworkTag) {
		t.Fatalf("instance = %+v", in)
	}
	if st := h.state("scout"); st.Generation != 1 || st.Power != driver.PowerAsleep || st.Running {
		t.Fatalf("staged state = %+v", st)
	}
	h.must(h.d.StageComputer(h.ctx, spec)) // same generation, same spec: nothing to do
	if calls := h.fake.Calls(); len(calls) != 0 {
		t.Fatalf("restaging the same generation called %v", calls)
	}

	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	st := h.state("scout")
	if st.Power != driver.PowerAwake || !st.Running || st.Address == "" {
		t.Fatalf("awake state = %+v", st)
	}
	if got, ok := h.d.Caller(netip.MustParseAddr("10.128.0.2"), netip.MustParseAddr(st.Address)); !ok || got != "scout" {
		t.Errorf("Caller(scout's VM address) = %q, %v", got, ok)
	}
	if v := h.guests.get("scout"); v.built != 1 {
		t.Errorf("waking an already built generation rebuilt it (%d builds)", v.built)
	}
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1)) // idempotent

	next := testSpec("scout", 2)
	next.CPUMillis, next.Memory = 2000, 8<<30
	if err := h.d.StageComputer(h.ctx, next); !errors.Is(err, driver.ErrRunning) {
		t.Fatalf("stage over a running VM = %v, want ErrRunning", err)
	}
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAsleep, 1))
	h.fake.Calls()
	h.must(h.d.StageComputer(h.ctx, next))
	if calls := h.fake.Calls(); !slices.Equal(calls, []string{"instances.setMachineType " + vmName, "instances.setLabels " + vmName}) {
		t.Fatalf("restage calls = %v; it must not boot the VM", calls)
	}
	if in, _ := h.fake.Instance(vmName); last(in.MachineType) != "e2-standard-2" || in.Labels[LabelGeneration] != "2" {
		t.Fatalf("restaged instance: %s, labels %v", in.MachineType, in.Labels)
	}
	if err := h.d.StageComputer(h.ctx, testSpec("scout", 1)); !errors.Is(err, driver.ErrStaleGeneration) {
		t.Fatalf("stage of an older generation = %v", err)
	}
	if err := h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1); !errors.Is(err, driver.ErrStaleGeneration) {
		t.Fatalf("wake of the old generation = %v", err)
	}
	if err := h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 3); !errors.Is(err, driver.ErrNotStaged) {
		t.Fatalf("wake of an unstaged generation = %v", err)
	}
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 2))
	if v := h.guests.get("scout"); v.built != 2 || v.spec.Generation != 2 || !v.running {
		t.Fatalf("wake after a restage: %+v", v)
	}

	if err := h.d.RemoveDrive(h.ctx, driver.HomeDriveID("scout")); !errors.Is(err, driver.ErrDriveAttached) {
		t.Fatalf("remove an attached drive = %v", err)
	}
	h.must(h.d.RemoveComputer(h.ctx, "scout"))
	h.must(h.d.RemoveComputer(h.ctx, "scout")) // already gone
	obs, err := h.d.Observe(h.ctx)
	h.must(err)
	if _, ok := obs.Computers["scout"]; ok {
		t.Fatal("removed Computer still observed")
	}
	if dr, ok := obs.Drives[driver.HomeDriveID("scout")]; !ok || len(dr.AttachedTo) != 0 {
		t.Fatalf("home drive after removal = %+v, %v", dr, ok)
	}
	h.must(h.d.RemoveDrive(h.ctx, driver.HomeDriveID("scout")))
	if _, ok := h.fake.Disk(DiskName("lab", "home-scout")); ok {
		t.Fatal("drive not removed")
	}
}

// A generation is staged only through GCE's compare-and-swap on the labels: a writer that staged a newer generation
// between the read and the write wins, and the older stage is refused.
func TestStageLosesToANewerGeneration(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	vmName := InstanceName("lab", "scout")
	h.fake.Before("instances.setLabels", func() { h.fake.SetLabel(vmName, LabelGeneration, "5") })
	if err := h.d.StageComputer(h.ctx, testSpec("scout", 2)); !errors.Is(err, driver.ErrStaleGeneration) {
		t.Fatalf("stage = %v, want ErrStaleGeneration after a concurrent newer stage", err)
	}
	if in, _ := h.fake.Instance(vmName); in.Labels[LabelGeneration] != "5" {
		t.Fatalf("generation = %s, want the newer writer's 5", in.Labels[LabelGeneration])
	}
}

// A spec the Controller no longer holds for the staged generation is never started.
func TestWakeNeedsTheStagedSpec(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	h.must(h.d.store.remove("scout"))
	if err := h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1); !errors.Is(err, driver.ErrNotStaged) {
		t.Fatalf("wake without the staged spec = %v, want ErrNotStaged", err)
	}
}

// A bad image or bundle fails the first stage, and the VM is not left running.
func TestFirstStageFailureStopsTheVM(t *testing.T) {
	h := newHarness(t)
	h.guests.failMat = errors.New("bundle codex@1.0.0 is not in the cache")
	_, err := h.d.EnsureDrive(h.ctx, driver.DriveSpec{ID: driver.HomeDriveID("scout"), Kind: driver.DriveHome, Computer: "scout"})
	h.must(err)
	if err := h.d.StageComputer(h.ctx, testSpec("scout", 1)); err == nil || !strings.Contains(err.Error(), "not in the cache") {
		t.Fatalf("stage = %v", err)
	}
	if in, _ := h.fake.Instance(InstanceName("lab", "scout")); in.Status != "TERMINATED" {
		t.Fatalf("VM is %s after a failed stage", in.Status)
	}
}

func TestDrives(t *testing.T) {
	h := newHarness(t)
	spec := driver.DriveSpec{ID: driver.HomeDriveID("scout"), Kind: driver.DriveHome, Computer: "scout", Size: 20 << 30}
	if created, err := h.d.EnsureDrive(h.ctx, spec); err != nil || !created {
		t.Fatalf("EnsureDrive = %v, %v", created, err)
	}
	spec.Size = 30<<30 + 1
	if created, err := h.d.EnsureDrive(h.ctx, spec); err != nil || created {
		t.Fatalf("EnsureDrive grow = %v, %v", created, err)
	}
	if disk, _ := h.fake.Disk(DiskName("lab", "home-scout")); disk.SizeGb != 31 {
		t.Fatalf("size = %d GiB, want 31 (rounded up)", disk.SizeGb)
	}
	spec.Size = 1 << 30
	h.must(func() error { _, err := h.d.EnsureDrive(h.ctx, spec); return err }())
	if disk, _ := h.fake.Disk(DiskName("lab", "home-scout")); disk.SizeGb != 31 {
		t.Fatalf("a smaller size shrank the disk to %d GiB", disk.SizeGb)
	}
	h.must(h.d.api.InsertDisk(h.ctx, &gceapi.Disk{Name: DiskName("lab", "home-x"), Labels: map[string]string{LabelFleet: "other"}}))
	if _, err := h.d.EnsureDrive(h.ctx, driver.DriveSpec{ID: driver.HomeDriveID("x"), Kind: driver.DriveHome, Computer: "x"}); err == nil {
		t.Fatal("a disk of another fleet was adopted")
	}
	if err := h.d.RemoveDrive(h.ctx, driver.HomeDriveID("x")); err == nil {
		t.Fatal("a disk of another fleet was removed")
	}
	if _, err := h.d.EnsureDrive(h.ctx, driver.DriveSpec{ID: "keep-x", Kind: driver.DriveKeep, Computer: "scout"}); err == nil {
		t.Fatal("a keep drive was accepted")
	}
}

func TestStageRefusesAttach(t *testing.T) {
	h := newHarness(t)
	spec := testSpec("scout", 1)
	spec.Attach = []driver.Attach{{Host: "/srv/app", Target: "/home/agent/app"}}
	if err := h.d.StageComputer(h.ctx, spec); err == nil || !strings.Contains(err.Error(), "host folders") {
		t.Fatalf("stage with attach = %v", err)
	}
}

func TestCapabilities(t *testing.T) {
	h := newHarness(t)
	caps, err := h.d.Capabilities(h.ctx)
	h.must(err)
	if !caps.EgressEnforcement || !caps.DriveQuota || caps.Services || caps.HostFolders || !slices.Equal(caps.Architectures, []string{"amd64"}) {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestFirewallCheck(t *testing.T) {
	h := newHarness(t)
	broken := map[string]func(fw []gceapi.Firewall) []gceapi.Firewall{
		"no egress deny": func(fw []gceapi.Firewall) []gceapi.Firewall { return fw[1:] },
		"no way to the Controller": func(fw []gceapi.Firewall) []gceapi.Firewall {
			return append(fw[:1:1], fw[2])
		},
		"egress beyond the Controller": func(fw []gceapi.Firewall) []gceapi.Firewall {
			fw[1].DestinationRanges = []string{"10.128.0.0/20"}
			return fw
		},
		"extra Controller port": func(fw []gceapi.Firewall) []gceapi.Firewall {
			fw[1].Allowed[0].Ports = append(fw[1].Allowed[0].Ports, "7000")
			return fw
		},
		"engine open to the subnet": func(fw []gceapi.Firewall) []gceapi.Firewall {
			fw[2].SourceServiceAccounts, fw[2].SourceRanges = nil, []string{"10.128.0.0/20"}
			return fw
		},
		"an allow-all ahead of the deny": func(fw []gceapi.Firewall) []gceapi.Firewall {
			return append(fw, gceapi.Firewall{Name: "allow-all", Network: "net", Priority: 500, Direction: "EGRESS",
				DestinationRanges: []string{"0.0.0.0/0"}, Allowed: []gceapi.FirewallRule{{IPProtocol: "all"}}})
		},
	}
	for name, mutate := range broken {
		h.fake.SetFirewalls(mutate(goodFirewall()))
		if _, err := h.d.Capabilities(h.ctx); err == nil || !strings.Contains(err.Error(), "fix:") {
			t.Errorf("%s: Capabilities = %v, want a refusal with a fix", name, err)
		}
	}
	fw := goodFirewall()
	fw = append(fw, gceapi.Firewall{Name: "other-network-allow-all", Network: "elsewhere", Priority: 1, Direction: "EGRESS",
		DestinationRanges: []string{"0.0.0.0/0"}, Allowed: []gceapi.FirewallRule{{IPProtocol: "all"}}})
	h.fake.SetFirewalls(fw)
	if _, err := h.d.Capabilities(h.ctx); err != nil {
		t.Errorf("a rule of another network counted: %v", err)
	}
}

func TestExecAndSessions(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	if _, _, err := h.d.Exec(h.ctx, "scout", []string{"true"}, nil, false); err == nil {
		t.Fatal("exec in a sleeping Computer succeeded")
	}
	if n, err := h.d.Sessions(h.ctx, "scout"); err != nil || n != 0 {
		t.Fatalf("sessions asleep = %d, %v", n, err)
	}
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	if code, _, err := h.d.Exec(h.ctx, "scout", []string{"id"}, nil, true); err != nil || code != 0 {
		t.Fatalf("exec = %d, %v", code, err)
	}
	if v := h.guests.get("scout"); len(v.execs) != 1 || !strings.HasPrefix(v.execs[0], "0:0") {
		t.Fatalf("execs = %v", v.execs)
	}
}

// A stage interrupted after it created the VM but before it stopped it leaves a running VM with no running
// Computer; the next stage stops it instead of refusing forever.
func TestStageStopsAnIdleRunningVM(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	h.must(h.d.api.StartInstance(h.ctx, InstanceName("lab", "scout")))
	if st := h.state("scout"); st.Running {
		t.Fatalf("state = %+v", st)
	}
	h.must(h.d.StageComputer(h.ctx, testSpec("scout", 2)))
	if in, _ := h.fake.Instance(InstanceName("lab", "scout")); in.Status != "TERMINATED" || in.Labels[LabelGeneration] != "2" {
		t.Fatalf("instance %s, generation %s", in.Status, in.Labels[LabelGeneration])
	}
}
