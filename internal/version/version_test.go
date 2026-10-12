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
