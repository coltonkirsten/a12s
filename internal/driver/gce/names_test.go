package gce

import (
	"regexp"
	"strings"
	"testing"
)

func TestNamesFitGCE(t *testing.T) {
	resource := regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	account := regexp.MustCompile(`^[a-z]([-a-z0-9]{4,28}[a-z0-9])$`)
	long := strings.Repeat("a", 63)
	for _, c := range []struct{ fleet, computer string }{
		{"lab", "scout"}, {"lab", long}, {long, long}, {"9lab", "x"}, {"l", "c"}, {"lab", "a-" + strings.Repeat("b", 30)},
	} {
		for _, n := range []string{InstanceName(c.fleet, c.computer), DiskName(c.fleet, "home-"+c.computer)} {
			if !resource.MatchString(n) {
				t.Errorf("%q is not a valid GCE resource name", n)
			}
		}
		if id := ServiceAccountID(c.fleet, c.computer); !account.MatchString(id) {
			t.Errorf("%q is not a valid service account ID", id)
		}
	}
	if InstanceName("lab", long+"x") == InstanceName("lab", long+"y") {
		t.Error("shortened names collide")
	}
	if ServiceAccountID("lab", strings.Repeat("b", 30)+"1") == ServiceAccountID("lab", strings.Repeat("b", 30)+"2") {
		t.Error("service account IDs collide")
	}
}
