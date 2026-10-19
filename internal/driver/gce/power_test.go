package gce

import (
	"errors"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// A zone out of capacity for the first family's shape falls back to the next family's shape of the same size. The
// next wake starts from the first family again.
func TestStockoutFallsBackToTheNextFamily(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	vm := InstanceName("lab", "scout")
	h.fake.FailOp("start", vm, "ZONE_RESOURCE_POOL_EXHAUSTED")
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	if in, _ := h.fake.Instance(vm); last(in.MachineType) != "n2d-standard-2" || in.Status != "RUNNING" {
		t.Fatalf("after a stockout: %s %s, want n2d-standard-2 running", in.MachineType, in.Status)
	}
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAsleep, 1))
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	if in, _ := h.fake.Instance(vm); last(in.MachineType) != "e2-small" {
		t.Fatalf("the next wake stayed on %s", in.MachineType)
	}
}

// With every family out of capacity the wake fails with GCE's code, and Observe says why until a wake succeeds.
func TestNoCapacityAnywhere(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	vm := InstanceName("lab", "scout")
	for range DefaultFamilies["amd64"] {
		h.fake.FailOp("start", vm, "ZONE_RESOURCE_POOL_EXHAUSTED")
	}
	err := h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1)
	if !gceapi.HasReason(err, "ZONE_RESOURCE_POOL_EXHAUSTED") || !strings.Contains(err.Error(), "e2-small, n2d-standard-2, t2d-standard-1") {
		t.Fatalf("wake = %v", err)
	}
	if st := h.state("scout"); st.Power != driver.PowerAsleep || !strings.Contains(st.Failure, "no capacity") {
		t.Fatalf("state = %+v", st)
	}
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	if in, _ := h.fake.Instance(vm); in.Labels[LabelFailure] != "" {
		t.Fatalf("a successful wake left failure %q", in.Labels[LabelFailure])
	}
}

// A VM that booted but can't run its Computer is stopped again, so it doesn't bill while the Controller retries.
func TestFailedWakeStopsTheVM(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	h.must(h.d.StageComputer(h.ctx, testSpec("scout", 2)))
	h.guests.failMat = errors.New("load image: no space left on device")
	if err := h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 2); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("wake = %v", err)
	}
	if in, _ := h.fake.Instance(InstanceName("lab", "scout")); in.Status != "TERMINATED" {
		t.Fatalf("VM is %s after a failed wake", in.Status)
	}
}

func TestFailureText(t *testing.T) {
	for label, want := range map[string]string{
		"oom": "out of memory", "exit-3": "exited with code 3", "host-error": "host error",
		"guest-terminate": "the VM shut itself down", "no-capacity": "zone z has no capacity for e2-small",
	} {
		if got, _, _ := failureText(label, "z", "e2-small"); got != want {
			t.Errorf("failureText(%q) = %q, want %q", label, got, want)
		}
	}
	if _, code, oom := failureText("oom", "z", "m"); code != 137 || !oom {
		t.Error("oom is not reported as an OOM kill")
	}
}

// A VM that is up bills, so it is awake even when its container isn't running (a missed death, a stop that failed
// half-way): the Controller's sleep then stops it.
func TestUpVMWithAStoppedContainerIsAwake(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	h.kill("scout")
	if st := h.state("scout"); st.Power != driver.PowerAwake || st.Running {
		t.Fatalf("state = %+v, want awake and not running", st)
	}
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAsleep, 1))
	if in, _ := h.fake.Instance(InstanceName("lab", "scout")); in.Status != "TERMINATED" {
		t.Fatalf("VM is %s after sleep", in.Status)
	}
}

// A VM suspended outside a12s wakes by resuming: GCE refuses to start or reshape a suspended VM.
func TestSuspendedVMResumes(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	h.fake.SetStatus(InstanceName("lab", "scout"), "SUSPENDED")
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	if st := h.state("scout"); !st.Running {
		t.Fatalf("state = %+v", st)
	}
}
