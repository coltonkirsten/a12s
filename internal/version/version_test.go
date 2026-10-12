package version

import "testing"

func TestRefsFollowTheRelease(t *testing.T) {
	defer func(v, r string) { Version, Registry = v, r }(Version, Registry)

	if got := Image("slim"); got != "a12s/slim:dev" {
		t.Fatalf("dev slim = %s", got)
	}
	if got := Service("svc-cron"); got != "a12s/svc-cron:0.1" {
		t.Fatalf("dev svc-cron = %s", got)
	}
	Version, Registry = "v0.1.0", "ghcr.io/coltonkirsten/a12s"
	for got, want := range map[string]string{
		Image("controller"):              "ghcr.io/coltonkirsten/a12s/controller:0.1.0",
		Service("svc-cron"):              "ghcr.io/coltonkirsten/a12s/svc-cron:0.1.0",
		Bundle("claude-code", "2.1.280"): "ghcr.io/coltonkirsten/a12s/bundle-claude-code:2.1.280",
	} {
		if got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
}

func TestCheckComputerd(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	for _, tc := range []struct {
		controller, label string
		ok, warn          bool
	}{
		{"v0.1.1", "v0.1.1", true, false},
		{"v0.1.1", "v0.1.0", true, false},
		{"v0.1.0", "v0.1.9-rc.1", true, false},
		{"v0.1.1", "v0.2.0", false, false},
		{"v0.2.0", "v0.1.0", false, false},
		{"v1.4.0", "v1.0.2", true, false},
		{"v1.4.0", "v2.0.0", false, false},
		{"v1.0.0", "v0.9.0", false, false},
		{"v0.1.1", "latest", false, false},
		{"v0.1.1", "dev", true, true},
		{"dev", "v0.1.0", true, true},
		{"dev", "dev", true, false},
	} {
		Version = tc.controller
		warn, err := CheckComputerd(tc.label)
		if (err == nil) != tc.ok || (warn != "") != tc.warn {
			t.Errorf("Controller %s, label %s: warn %q, err %v", tc.controller, tc.label, warn, err)
		}
	}
}
