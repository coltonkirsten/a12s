package gce

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// With per-computer identity each Computer's VM runs as its own keyless, role-less service account, created before
// the VM and retried while IAM propagates it, reported by Observe, and deleted with the Computer.
func TestPerComputerIdentity(t *testing.T) {
	identityRetry = []time.Duration{0, 0, 0}
	h := newHarness(t, func(c *Config) { c.Identity = IdentityPerComputer })
	email := ServiceAccountEmail("lab", "scout", testProject)
	vm := InstanceName("lab", "scout")
	h.fake.FailHTTP("instances.insert", vm, http.StatusBadRequest, "invalid",
		"The resource 'projects/p/serviceAccounts/"+email+"' was not found")
	h.stageNew("scout")
	if got := h.fake.ServiceAccounts(); !slices.Equal(got, []string{email}) {
		t.Fatalf("service accounts = %v", got)
	}
	in, _ := h.fake.Instance(vm)
	if len(in.ServiceAccounts) != 1 || in.ServiceAccounts[0].Email != email || !slices.Equal(in.ServiceAccounts[0].Scopes, []string{cloudPlatformScope}) {
		t.Fatalf("instance runs as %+v", in.ServiceAccounts)
	}
	if st := h.state("scout"); st.Identity != email {
		t.Fatalf("observed identity %q", st.Identity)
	}
	h.must(h.d.RemoveComputer(h.ctx, "scout"))
	if got := h.fake.ServiceAccounts(); len(got) != 0 {
		t.Fatalf("service account outlived its Computer: %v", got)
	}
	h.must(h.d.RemoveComputer(h.ctx, "scout")) // account already gone
}

func TestNoIdentityByDefault(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	if in, _ := h.fake.Instance(InstanceName("lab", "scout")); len(in.ServiceAccounts) != 0 || len(h.fake.ServiceAccounts()) != 0 {
		t.Fatalf("a Computer without identity got an account: %+v", in.ServiceAccounts)
	}
	if st := h.state("scout"); st.Identity != "" || st.Power != driver.PowerAsleep {
		t.Fatalf("state = %+v", st)
	}
}
