package fleet

import (
	"errors"
	"fmt"
	"path"
	"regexp"
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

var reservedPaths = []string{"/home", "/opt/a12s", "/run", "/tmp", "/proc", "/sys", "/dev", "/etc", "/usr", "/bin", "/sbin", "/lib", "/var"}

func ValidName(s string) bool { return nameRE.MatchString(s) }

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
	switch f.Egress.Enforcement {
	case "", EnforcementEnforced, EnforcementUnenforced:
	default:
		bad("egress.enforcement", "must be %q or %q, got %q", EnforcementEnforced, EnforcementUnenforced, f.Egress.Enforcement)
	}

	for _, name := range f.DriveNames() {
		d := f.Drives[name]
		field := "drives." + name
		if !nameRE.MatchString(name) {
			bad(field, "name must be a lowercase DNS label of at most 40 characters")
		}
		switch d.Access {
		case AccessExclusive, AccessShared, AccessSharedWrite:
		default:
			bad(field+".access", "must be %s, %s or %s, got %q", AccessExclusive, AccessShared, AccessSharedWrite, d.Access)
		}
	}

	errs = append(errs, validateInbox("defaults.inbox", f.Defaults.Inbox)...)
	isComputer := func(n string) bool { _, ok := f.Computers[n]; return ok }
	isService := f.hasService
	errs = append(errs, validatePower("defaults.power", f.Defaults.Power, isComputer, isService)...)

	attached := map[string][]string{} // drive -> computers
	for _, name := range f.ComputerNames() {
		c := f.Computers[name]
		field := "agent_computers." + name
		if !nameRE.MatchString(name) {
			bad(field, "name must be a lowercase DNS label of at most 40 characters")
		}
		if name == "controller" {
			bad(field, "\"controller\" is reserved for the Controller's certificate identity")
		}
		errs = append(errs, c.validate(field, f.Drives)...)
		errs = append(errs, validateInbox(field+".inbox", c.Inbox)...)
		errs = append(errs, validatePower(field+".power", c.Power, isComputer, isService)...)
		errs = append(errs, f.validateUses(field, "", c.Uses)...)
		errs = append(errs, f.validateGrants(field, c.Grants)...)
		for _, m := range c.Drives {
			if _, ok := f.Drives[m.Name]; ok {
				attached[m.Name] = append(attached[m.Name], name)
			}
		}
	}
	errs = append(errs, f.validateServices()...)
	errs = append(errs, f.validateAllowPrivate()...)
	for _, d := range f.DriveNames() {
		if f.Drives[d].Access == AccessExclusive && len(attached[d]) > 1 {
			bad("drives."+d, "exclusive drive is attached to more than one Computer: %s", strings.Join(attached[d], ", "))
		}
	}
	return errors.Join(errs...)
}

// removedBrowserTier was a built-in image before 0.3.0; naming it gets a migration hint instead of a pull of
// docker.io/library/browser.
const removedBrowserTier = "browser"

func (c Computer) validate(field string, drives map[string]Drive) []error {
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
	for _, msg := range validateFiles(c) {
		errs = append(errs, fmt.Errorf("%s.%s", field, msg))
	}
	switch c.Autonomy {
	case AutonomyFull, AutonomyRestricted:
	default:
		bad("autonomy", "must be %s or %s, got %q", AutonomyFull, AutonomyRestricted, c.Autonomy)
	}
	if c.Home.Preset != "" && (path.IsAbs(c.Home.Preset) || strings.Contains(c.Home.Preset, "\\")) {
		bad("home.preset", "%q must be a path relative to the Fleet file", c.Home.Preset)
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

	seenDrive, seenAt := map[string]bool{}, map[string]bool{}
	for i, m := range c.Drives {
		sub := fmt.Sprintf("drives[%d]", i)
		d, ok := drives[m.Name]
		switch {
		case !ok:
			bad(sub+".name", "drive %q is not declared under drives", m.Name)
		case d.Access == AccessShared && !m.ReadOnly:
			bad(sub, "shared drive %q must be attached readOnly", m.Name)
		}
		if seenDrive[m.Name] {
			bad(sub+".name", "drive %q is attached twice", m.Name)
		}
		seenDrive[m.Name] = true
		if err := checkMountPoint(m.At); err != nil {
			bad(sub+".at", "%v", err)
		} else if seenAt[m.At] {
			bad(sub+".at", "%q is used twice", m.At)
		}
		seenAt[m.At] = true
	}

	for _, h := range c.Egress.Allow {
		if h != AllowAll && !hostRE.MatchString(h) {
			bad("egress.allow", "%q must be a lowercase host name (no scheme, port or wildcard) or %q", h, AllowAll)
		}
	}
	return errs
}

func checkMountPoint(at string) error {
	if !mountRE.MatchString(at) {
		// Mount points travel in A12S_DRIVES as "<at>:<mode>,..."; ',' and ':' would forge entries.
		return fmt.Errorf("%q may only contain letters, digits, '.', '_', '-' and '/'", at)
	}
	if !path.IsAbs(at) || path.Clean(at) != at {
		return fmt.Errorf("%q must be a clean absolute path", at)
	}
	if at == "/" {
		return fmt.Errorf("%q is reserved", at)
	}
	for _, r := range reservedPaths {
		if at == r || strings.HasPrefix(at, r+"/") {
			return fmt.Errorf("%q is under reserved path %s", at, r)
		}
	}
	return nil
}
