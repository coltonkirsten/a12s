// Package browser exposes the browser tier's seccomp profile to the
// Controller, which passes it to the Driver when staging a browser Computer.
package browser

import _ "embed"

// Seccomp is images/browser/seccomp.json: Docker's default profile plus the
// user-namespace calls Chromium's own sandbox needs.
//
//go:embed seccomp.json
var Seccomp string
