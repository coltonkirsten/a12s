package fleet

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/distribution/reference"
)

var (
	// nameRE is a DNS label; names become Docker and Kubernetes object names.
	nameRE       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	versionRE    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	credentialRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	mountRE      = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	hostRE       = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func ValidName(s string) bool { return nameRE.MatchString(s) }

func ValidCredential(s string) bool { return credentialRE.MatchString(s) }

func (f *Fleet) Validate() error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if f.APIVersion != APIVersion {
		bad("apiVersion", "must be %q, got %q", APIVersion, f.APIVersion)
	}
	if f.Kind != Kind {
		bad("kind", "must be %q, got %q", Kind, f.Kind)
	}
	if !nameRE.MatchString(f.Metadata.Name) {
		bad("metadata.name", "%q must be a lowercase DNS label of at most 40 characters", f.Metadata.Name)
	}
	errs = append(errs, validateInbox("defaults.inbox", f.Defaults.Inbox)...)
	isComputer := func(n string) bool { _, ok := f.Computers[n]; return ok }
	isService := f.hasService
	errs = append(errs, validatePower("defaults.power", f.Defaults.Power, isComputer, isService)...)
	if len(f.Defaults.Attach) > 0 {
		bad("defaults.attach", "an attached folder belongs to one agent computer; move it under agent_computers.<name>.attach")
	}

	for _, name := range f.ComputerNames() {
		c := f.Computers[name]
		field := "agent_computers." + name
		if !nameRE.MatchString(name) {
			bad(field, "name must be a lowercase DNS label of at most 40 characters")
		}
		if name == "controller" {
			bad(field, "\"controller\" is reserved for the Controller's certificate identity")
		}
		errs = append(errs, c.validate(field)...)
		errs = append(errs, validateInbox(field+".inbox", c.Inbox)...)
		errs = append(errs, validatePower(field+".power", c.Power, isComputer, isService)...)
		errs = append(errs, f.validateUses(field, "", c.Uses)...)
		errs = append(errs, f.validateGrants(field, c.Grants)...)
	}
	errs = append(errs, f.validateServices()...)
	return errors.Join(errs...)
}

// removedBrowserTier was a built-in image before 0.3.0; naming it gets a migration hint instead of a pull of
// docker.io/library/browser.
const removedBrowserTier = "browser"

func (c Computer) validate(field string) []error {
	var errs []error
	bad := func(sub, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s.%s: %s", field, sub, fmt.Sprintf(format, args...)))
	}

	switch c.Harness.Kind {
	case HarnessClaudeCode, HarnessCodex:
	default:
		bad("harness.kind", "must be %s or %s, got %q", HarnessClaudeCode, HarnessCodex, c.Harness.Kind)
	}
	if !versionRE.MatchString(c.Harness.Version) {
		bad("harness.version", "%q is not a valid version", c.Harness.Version)
	}
	switch {
	case c.Image == "":
		bad("image", "required: %s or an image reference", TierSlim)
	case c.Image == removedBrowserTier:
		bad("image", "the browser image is no longer part of a12s: build one FROM %s with Chromium and set security.seccomp (docs/images.md#custom-images)", TierSlim)
	case c.Tier() == TierCustom:
		if _, err := reference.ParseAnyReference(c.Image); err != nil {
			bad("image", "%q is neither %s nor a valid image reference: %v", c.Image, TierSlim, err)
		}
	}
	if p := c.Security.Seccomp.Path; p != "" && (path.IsAbs(p) || strings.Contains(p, "\\")) {
		bad("security.seccomp", "%q must be a path relative to the Fleet file", p)
	}
	if prof := c.Security.Seccomp.Profile; len(prof) > 0 {
		if _, err := ValidateSeccomp(prof); err != nil {
			bad("security.seccomp", "%v", err)
		}
	}
	for _, msg := range slices.Concat(validateFiles(c), validateAttach(c)) {
		errs = append(errs, fmt.Errorf("%s.%s", field, msg))
	}

	seenCred := map[string]bool{}
	for _, cred := range c.Credentials {
		if !credentialRE.MatchString(cred) {
			bad("credentials", "%q is not a valid credential name", cred)
		}
		if seenCred[cred] {
			bad("credentials", "%q is listed twice", cred)
		}
		seenCred[cred] = true
	}

	for _, h := range c.Egress.Allow {
		if h != AllowAll && !hostRE.MatchString(h) {
			bad("egress.allow", "%q must be a lowercase host name (no scheme, port or wildcard) or %q", h, AllowAll)
		}
	}
	return errs
}
