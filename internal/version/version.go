// Package version names the a12s release a binary was built as and the image references that release resolves.
//
// A source build is "dev" and resolves local images (a12s/slim:dev, built by `make images`). A release build sets
// Version and Registry with -ldflags, so the binary and the Controller inside its image both pull the images
// published with that same release, never a local build.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

var (
	Version  = "dev"
	Registry = "a12s"
)

// DevServiceTag is the tag `make services` gives the reference Services in a source build.
const DevServiceTag = "0.1"

// Release reports whether this binary was built by the release pipeline.
func Release() bool { return Version != "dev" }

// tag is the image tag of this release: "0.1.0" for v0.1.0.
func tag() string { return strings.TrimPrefix(Version, "v") }

// Image is an a12s-built image of this release: slim, browser, controller.
func Image(name string) string { return Registry + "/" + name + ":" + tag() }

// Service is a reference Service image of this release: svc-cron, svc-mcp, svc-mcp-postgres, svc-webhook.
func Service(name string) string {
	if !Release() {
		return Registry + "/" + name + ":" + DevServiceTag
	}
	return Image(name)
}

// Bundle is a HarnessBundle; bundles are versioned by the harness, not by a12s.
func Bundle(kind, harnessVersion string) string {
	return Registry + "/bundle-" + kind + ":" + harnessVersion
}

// CheckComputerd compares the release a custom Computer image's computerd came from (its
// ai.a12s.computerd.version label) with this binary's. A release accepts the same major version, and within 0.x
// the same minor, since 0.x minors may change the Controller–computerd API. Either side being a dev build only
// warns: nothing pins what a dev build speaks.
func CheckComputerd(label string) (warning string, err error) {
	if label == Version {
		return "", nil
	}
	if !Release() || label == "dev" {
		return fmt.Sprintf("computerd is from a12s %s and this Controller is %s; a dev build skips the compatibility check", label, Version), nil
	}
	have, ok := parseRelease(label)
	if !ok {
		return "", fmt.Errorf("its computerd version %q is not an a12s release", label)
	}
	want, ok := parseRelease(Version)
	if !ok {
		return fmt.Sprintf("computerd is from a12s %s and this Controller is %s, which is not a release version; compatibility is not checked", label, Version), nil
	}
	if have[0] != want[0] || (want[0] == 0 && have[1] != want[1]) {
		return "", fmt.Errorf("its computerd is from a12s %s, which this Controller (%s) cannot drive", label, Version)
	}
	return "", nil
}

// parseRelease reads "v1.2.3" (or "1.2.3", with any -pre or +build suffix) as major, minor.
func parseRelease(v string) ([2]int, bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [2]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [2]int{}, false
		}
		out[i] = n
	}
	return [2]int{out[0], out[1]}, true
}
// warm CI timing probe; do not merge
