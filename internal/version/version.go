// Package version names the a12s release a binary was built as and the image references that release resolves.
//
// A source build is "dev" and resolves local images (a12s/slim:dev, built by `make images`). A release build sets
// Version and Registry with -ldflags, so the binary and the Controller inside its image both pull the images
// published with that same release, never a local build.
package version

import "strings"

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
