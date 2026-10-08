package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
)

// Service is a non-agent container the Controller runs from the Fleet file
// (enhancements §5). A Service inherits nothing from defaults: its own
// fields are complete, and the image manifest (or the inline manifest)
// fills what the file doesn't say.
type Service struct {
	// Image is required and pinned in fleet.lock.
	Image string `yaml:"image" json:"image"`
	// Manifest supplies or overrides the image's /a12s/service.json.
	Manifest *ServiceManifest `yaml:"manifest,omitempty" json:"manifest,omitempty"`
	// Config is portable, non-secret data, given to the Service as
	// /run/a12s/config/config.json. Its "env" map is exported into the
	// Service's environment by a12s-init.
	Config      map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
	Credentials []string       `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	Egress      ServiceEgress  `yaml:"egress,omitempty" json:"egress,omitzero"`
	Grants      Grants         `yaml:"grants,omitempty" json:"grants,omitzero"`
	Uses        []Use          `yaml:"uses,omitempty" json:"uses,omitempty"`
	Resources   Resources      `yaml:"resources,omitempty" json:"resources,omitzero"`
	State       ServiceState   `yaml:"state,omitempty" json:"state,omitzero"`
	// Publish is "<ip>:<hostPort>:<port>": the only ingress a12s has.
	Publish []string `yaml:"publish,omitempty" json:"publish,omitempty"`
}

// ServiceEgress is a Service's egress: the allowlist a Computer has, plus
// named TCP destinations (Services only).
type ServiceEgress struct {
	Allow []string    `yaml:"allow" json:"allow"`
	TCP   []TCPEgress `yaml:"tcp,omitempty" json:"tcp,omitempty"`
}

// AllowsAll reports whether the allowlist contains "*".
func (e ServiceEgress) AllowsAll() bool { return Egress{Allow: e.Allow}.AllowsAll() }

// TCPEgress is one named external TCP destination, reachable from the
// Service as <name>.egress.a12s:<port>.
type TCPEgress struct {
	Name string `yaml:"name" json:"name"`
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
}

// Grants are capabilities written on the side that acts (§5.3).
type Grants struct {
	// Send lists the Computers whose inboxes this may enqueue to.
	Send []string `yaml:"send,omitempty" json:"send,omitempty"`
}

// ServiceState overrides the manifest's state drive size.
type ServiceState struct {
	Size Bytes `yaml:"size,omitempty" json:"size,omitzero"`
}

// Use is one entry of `uses`: a Service name, and whether its MCP ports are
// registered with the harness (default yes). Written as a bare name or as
// {name, mcp}.
type Use struct {
	Name string `json:"name"`
	MCP  *bool  `json:"mcp,omitempty"`
}

// WantsMCP reports whether the Service's MCP ports are wired in.
func (u Use) WantsMCP() bool { return u.MCP == nil || *u.MCP }

// UnmarshalYAML accepts "db" or {name: db, mcp: false}.
func (u *Use) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		u.Name, u.MCP = n.Value, nil
		return nil
	case yaml.MappingNode:
		*u = Use{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			switch k {
			case "name":
				u.Name = v.Value
			case "mcp":
				var b bool
				if err := v.Decode(&b); err != nil {
					return fmt.Errorf("uses: mcp: %w", err)
				}
				u.MCP = &b
			default:
				return fmt.Errorf("uses: unknown field %q (a uses entry takes name and mcp)", k)
			}
		}
		return nil
	}
	return errors.New("uses: each entry is a Service name or {name, mcp}")
}

// MarshalYAML writes the bare name unless mcp is set.
func (u Use) MarshalYAML() (any, error) {
	if u.MCP == nil {
		return u.Name, nil
	}
	return map[string]any{"name": u.Name, "mcp": *u.MCP}, nil
}

// MarshalJSON writes the bare name unless mcp is set.
func (u Use) MarshalJSON() ([]byte, error) {
	if u.MCP == nil {
		return json.Marshal(u.Name)
	}
	type plain Use
	return json.Marshal(plain(u))
}

// UnmarshalJSON accepts "db" or {"name": "db", "mcp": false}.
func (u *Use) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*u = Use{Name: s}
		return nil
	}
	type plain Use
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*u = Use(p)
	return nil
}

// UseNames returns the Service names of a uses list.
func UseNames(us []Use) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Name
	}
	return out
}

// ServiceNames returns the Service names in sorted order.
func (f *Fleet) ServiceNames() []string { return sortedKeys(f.Services) }

// hasService reports whether the Fleet declares a Service of that name.
func (f *Fleet) hasService(name string) bool { _, ok := f.Services[name]; return ok }

// HasService reports whether the Fleet declares a Service of that name.
func (f *Fleet) HasService(name string) bool { return f.hasService(name) }

// serviceForbidden are the fields a Service may not have, with the reason
// and the fix (§3 "Rules that follow from the file").
var serviceForbidden = []struct{ key, why, fix string }{
	{"harness", "a Service runs no harness", "remove it; a Service that wants a harness is a Computer"},
	{"home", "a Service has no home drive", "remove it; use the manifest's state drive for data that must persist"},
	{"autonomy", "a Service runs no agent, so it has no autonomy", "remove it"},
	{"inbox", "a Service has no inbox", "remove it"},
	{"power", "a Service has no power policy (no auto-sleep, no wake on message)", "remove it; a12s sleep and a12s wake still work on Services"},
	{"drives", "a Service attaches no Fleet drives", "remove it; use the manifest's state drive"},
}

// decodeService decodes one services.<name> node, refusing Computer-only
// fields with a fix line rather than yaml's "field not found".
func decodeService(name string, n *yaml.Node, s *Service) error {
	n = mapping(n)
	var errs []error
	if n.Kind == yaml.MappingNode {
		for _, f := range serviceForbidden {
			if indexKey(n, f.key) >= 0 {
				errs = append(errs, fmt.Errorf("services.%s.%s: %s  fix: %s", name, f.key, f.why, f.fix))
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	if err := decodeStrict(data, s); err != nil && !errors.Is(err, errEmptyDocument) {
		return fmt.Errorf("services.%s: %w", name, err)
	}
	return nil
}

// computerOnlyEgress refuses egress.tcp on a Computer with a fix line.
func computerOnlyEgress(name string, n *yaml.Node) error {
	n = mapping(n)
	if n.Kind != yaml.MappingNode {
		return nil
	}
	i := indexKey(n, "egress")
	if i < 0 {
		return nil
	}
	if e := mapping(n.Content[i+1]); e.Kind == yaml.MappingNode && indexKey(e, "tcp") >= 0 {
		return fmt.Errorf("computers.%s.egress.tcp: only a Service can declare TCP egress  fix: put the login in a Service that holds it and add it to computers.%s.uses", name, name)
	}
	return nil
}

var (
	publishRE  = regexp.MustCompile(`^([0-9.]+):([0-9]{1,5}):([0-9]{1,5})$`)
	tcpNameRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	reservedFn = map[string]bool{"controller": true}
)

// Publish is one parsed publish entry.
type Publish struct {
	IP       netip.Addr
	HostPort int
	Port     int
}

// ParsePublish parses "127.0.0.1:8787:8080". The address must be written
// out: 127.0.0.1, or 0.0.0.0 (or another host address) explicitly.
func ParsePublish(s string) (Publish, error) {
	m := publishRE.FindStringSubmatch(s)
	if m == nil {
		return Publish{}, fmt.Errorf("%q must be \"127.0.0.1:<hostPort>:<port>\"", s)
	}
	ip, err := netip.ParseAddr(m[1])
	if err != nil || !ip.Is4() {
		return Publish{}, fmt.Errorf("%q: %q is not an IPv4 address", s, m[1])
	}
	hp, _ := strconv.Atoi(m[2])
	p, _ := strconv.Atoi(m[3])
	if hp < 1 || hp > 65535 || p < 1 || p > 65535 {
		return Publish{}, fmt.Errorf("%q: ports must be 1 to 65535", s)
	}
	return Publish{IP: ip, HostPort: hp, Port: p}, nil
}

// validateServices checks the parts of services: that need no image.
func (f *Fleet) validateServices() []error {
	var errs []error
	bad := func(field, fix, format string, args ...any) {
		msg := fmt.Sprintf("%s: %s", field, fmt.Sprintf(format, args...))
		if fix != "" {
			msg += "  fix: " + fix
		}
		errs = append(errs, errors.New(msg))
	}
	for _, name := range f.ServiceNames() {
		s := f.Services[name]
		field := "services." + name
		if !nameRE.MatchString(name) {
			bad(field, "", "name must be a lowercase DNS label of at most 40 characters")
		}
		if reservedFn[name] {
			bad(field, "rename it", "%q is reserved for the Controller's certificate identity", name)
		}
		if _, ok := f.Computers[name]; ok {
			bad(field, "rename one of them", "%s is already a Computer; Computers and Services share one <name>.a12s namespace", name)
		}
		switch {
		case s.Image == "":
			bad(field+".image", "set an image reference", "required")
		case s.Image == TierSlim || s.Image == TierBrowser:
			bad(field+".image", "use the Service's own image reference", "%q is a Computer image tier; a Service runs its own image", s.Image)
		default:
			if _, err := reference.ParseAnyReference(s.Image); err != nil {
				bad(field+".image", "", "%q is not a valid image reference: %v", s.Image, err)
			}
		}
		seen := map[string]bool{}
		for _, cred := range s.Credentials {
			if !credentialRE.MatchString(cred) {
				bad(field+".credentials", "", "%q is not a valid credential name", cred)
			}
			if seen[cred] {
				bad(field+".credentials", "", "%q is listed twice", cred)
			}
			seen[cred] = true
		}
		for _, h := range s.Egress.Allow {
			if h != AllowAll && !hostRE.MatchString(h) {
				bad(field+".egress.allow", "", "%q must be a lowercase host name (no scheme, port or wildcard) or %q", h, AllowAll)
			}
		}
		seenTCP := map[string]bool{}
		for i, t := range s.Egress.TCP {
			sub := fmt.Sprintf("%s.egress.tcp[%d]", field, i)
			if !tcpNameRE.MatchString(t.Name) {
				bad(sub+".name", "", "%q must be a lowercase DNS label", t.Name)
			}
			if seenTCP[t.Name] {
				bad(sub+".name", "", "%q is used twice", t.Name)
			}
			seenTCP[t.Name] = true
			if _, err := netip.ParseAddr(t.Host); err != nil && !hostRE.MatchString(t.Host) {
				bad(sub+".host", "", "%q must be a host name or an IP address", t.Host)
			}
			if t.Port < 1 || t.Port > 65535 {
				bad(sub+".port", "", "%d must be 1 to 65535", t.Port)
			}
		}
		errs = append(errs, f.validateGrants(field, s.Grants)...)
		errs = append(errs, f.validateUses(field, name, s.Uses)...)
		seenHost := map[int]bool{}
		for _, p := range s.Publish {
			pub, err := ParsePublish(p)
			if err != nil {
				bad(field+".publish", "write it as \"127.0.0.1:<hostPort>:<port>\"", "%v", err)
				continue
			}
			if seenHost[pub.HostPort] {
				bad(field+".publish", "", "host port %d is published twice", pub.HostPort)
			}
			seenHost[pub.HostPort] = true
		}
		if s.Manifest != nil {
			errs = append(errs, s.Manifest.validatePartial(field+".manifest")...)
		}
	}
	return errs
}

// validateGrants checks a grants block: send names only Computers.
func (f *Fleet) validateGrants(field string, g Grants) []error {
	var errs []error
	seen := map[string]bool{}
	for _, to := range g.Send {
		switch {
		case to == AllowAll || strings.Contains(to, "*"):
			errs = append(errs, fmt.Errorf("%s.grants.send: %q: there is no wildcard  fix: name each Computer", field, to))
		case f.hasService(to):
			errs = append(errs, fmt.Errorf("%s.grants.send: %s is a Service, and only Computers have inboxes  fix: remove it from %s.grants.send", field, to, field))
		default:
			if _, ok := f.Computers[to]; !ok {
				errs = append(errs, fmt.Errorf("%s.grants.send: there is no Computer %s  fix: add computers.%s or remove it from %s.grants.send", field, to, to, field))
			}
		}
		if seen[to] {
			errs = append(errs, fmt.Errorf("%s.grants.send: %s is listed twice", field, to))
		}
		seen[to] = true
	}
	return errs
}

// validateUses checks a uses list: names only Services, never itself.
func (f *Fleet) validateUses(field, self string, uses []Use) []error {
	var errs []error
	seen := map[string]bool{}
	who := strings.TrimPrefix(strings.TrimPrefix(field, "computers."), "services.")
	for _, u := range uses {
		switch {
		case u.Name == self:
			errs = append(errs, fmt.Errorf("%s.uses: a Service can't use itself  fix: remove %s from %s.uses", field, self, field))
		case !f.hasService(u.Name):
			if _, isComputer := f.Computers[u.Name]; isComputer {
				errs = append(errs, fmt.Errorf("%s.uses: %s is a Computer; uses reaches Services only, and the inbox is the only way to a Computer  fix: remove it, or add it to %s.grants.send", field, u.Name, field))
			} else {
				errs = append(errs, fmt.Errorf("%s uses %s, but there is no Service %s  fix: add services.%s or remove it from %s.uses", who, u.Name, u.Name, u.Name, field))
			}
		}
		if seen[u.Name] {
			errs = append(errs, fmt.Errorf("%s.uses: %s is listed twice", field, u.Name))
		}
		seen[u.Name] = true
	}
	return errs
}

// a12sPaths are where a Service's state and tmpfs paths may not go.
var a12sPaths = []string{"/run/a12s", "/tmp", "/a12s", "/proc", "/sys", "/dev"}

func checkServicePath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return fmt.Errorf("%q must be a clean absolute path other than /", p)
	}
	if !mountRE.MatchString(p) {
		return fmt.Errorf("%q may only contain letters, digits, '.', '_', '-' and '/'", p)
	}
	for _, r := range a12sPaths {
		if p == r || strings.HasPrefix(p, r+"/") || strings.HasPrefix(r, p+"/") {
			return fmt.Errorf("%q overlaps %s, which a12s mounts", p, r)
		}
	}
	return nil
}
