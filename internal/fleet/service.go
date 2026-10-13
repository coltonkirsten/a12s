package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
)

// Service inherits nothing from defaults: the image (or inline) manifest fills
// what the file doesn't say.
type Service struct {
	Image    string           `yaml:"image" json:"image"`
	Manifest *ServiceManifest `yaml:"manifest,omitempty" json:"manifest,omitempty"`
	// Config must be non-secret; it lands in /run/a12s/config/config.json and
	// its "env" map is exported into the Service's environment by a12s-init.
	Config      map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
	Credentials []string       `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	Egress      ServiceEgress  `yaml:"egress,omitempty" json:"egress,omitzero"`
	Grants      Grants         `yaml:"grants,omitempty" json:"grants,omitzero"`
	Uses        []Use          `yaml:"uses,omitempty" json:"uses,omitempty"`
	Resources   Resources      `yaml:"resources,omitempty" json:"resources,omitzero"`
	State       ServiceState   `yaml:"state,omitempty" json:"state,omitzero"`
	// Publish entries are "<ip>:<hostPort>:<port>": the only ingress a12s has.
	Publish []string `yaml:"publish,omitempty" json:"publish,omitempty"`
}

type ServiceEgress struct {
	Allow []string    `yaml:"allow" json:"allow"`
	TCP   []TCPEgress `yaml:"tcp,omitempty" json:"tcp,omitempty"`
}

func (e ServiceEgress) AllowsAll() bool { return Egress{Allow: e.Allow}.AllowsAll() }

// TCPEgress is reachable from the Service as <name>.egress.a12s:<port>.
type TCPEgress struct {
	Name string `yaml:"name" json:"name"`
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
}

// Grants are written on the side that acts.
type Grants struct {
	Send []string `yaml:"send,omitempty" json:"send,omitzero"`
}

type ServiceState struct {
	Size Bytes `yaml:"size,omitempty" json:"size,omitzero"`
}

// Use is written as a bare name or {name, mcp}; MCP defaults to yes.
type Use struct {
	Name string `json:"name"`
	MCP  *bool  `json:"mcp,omitempty"`
}

func (u Use) WantsMCP() bool { return u.MCP == nil || *u.MCP }

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

func (u Use) MarshalYAML() (any, error) {
	if u.MCP == nil {
		return u.Name, nil
	}
	return map[string]any{"name": u.Name, "mcp": *u.MCP}, nil
}

func (u Use) MarshalJSON() ([]byte, error) {
	if u.MCP == nil {
		return json.Marshal(u.Name)
	}
	type plain Use
	return json.Marshal(plain(u))
}

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

func UseNames(us []Use) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Name
	}
	return out
}

func (f *Fleet) ServiceNames() []string { return sortedKeys(f.Services) }

func (f *Fleet) hasService(name string) bool { _, ok := f.Services[name]; return ok }

func (f *Fleet) HasService(name string) bool { return f.hasService(name) }

var serviceForbidden = []struct{ key, why, fix string }{
	{"harness", "a Service runs no harness", "remove it; a Service that wants a harness is a Computer"},
	{"home", "a Service has no home drive", "remove it; use the manifest's state drive for data that must persist"},
	{"autonomy", "a Service runs no agent, so it has no autonomy", "remove it"},
	{"inbox", "a Service has no inbox", "remove it"},
	{"power", "a Service has no power policy (no auto-sleep, no wake on message)", "remove it; a12s sleep and a12s wake still work on Services"},
	{"drives", "a Service attaches no Fleet drives", "remove it; use the manifest's state drive"},
}

// decodeService refuses Computer-only fields with a fix line rather than yaml's
// "field not found".
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
		return fmt.Errorf("agent_computers.%s.egress.tcp: only a Service can declare TCP egress  fix: put the login in a Service that holds it and add it to agent_computers.%s.uses", name, name)
	}
	return nil
}

var (
	publishRE  = regexp.MustCompile(`^([0-9.]+):([0-9]{1,5}):([0-9]{1,5})$`)
	tcpNameRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	reservedFn = map[string]bool{"controller": true}
)

type Publish struct {
	IP       netip.Addr
	HostPort int
	Port     int
}

// ParsePublish requires the address written out, so exposing on 0.0.0.0 is
// always explicit.
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
		case s.Image == TierSlim:
			bad(field+".image", "use the Service's own image reference", "%q is the Computer base image; a Service runs its own image", s.Image)
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
			if a, err := netip.ParseAddr(t.Host); err == nil && isPrivate(a) && !f.allowsPrivate(a) {
				bad(sub+".host", "add a range covering it to egress.allowPrivate (top level), or use a public host", "%s is a private address", t.Host)
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
				errs = append(errs, fmt.Errorf("%s.grants.send: there is no agent computer %s  fix: add agent_computers.%s or remove it from %s.grants.send", field, to, to, field))
			}
		}
		if seen[to] {
			errs = append(errs, fmt.Errorf("%s.grants.send: %s is listed twice", field, to))
		}
		seen[to] = true
	}
	return errs
}

func (f *Fleet) validateUses(field, self string, uses []Use) []error {
	var errs []error
	seen := map[string]bool{}
	who := strings.TrimPrefix(strings.TrimPrefix(field, "agent_computers."), "services.")
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

func isPrivate(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

func (f *Fleet) allowsPrivate(a netip.Addr) bool {
	for _, s := range f.Egress.AllowPrivate {
		if p, err := netip.ParsePrefix(s); err == nil && p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

func (f *Fleet) validateAllowPrivate() []error {
	var errs []error
	for _, s := range f.Egress.AllowPrivate {
		p, err := netip.ParsePrefix(s)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("egress.allowPrivate: %q is not a CIDR  fix: write it as 10.0.0.0/8", s))
		case p.Bits() == 0:
			errs = append(errs, fmt.Errorf("egress.allowPrivate: %q opens everything  fix: name the private range the Service needs", s))
		}
	}
	return errs
}

// GrantWarnings: a principal in a power policy that can't send to the
// Computer never wakes it.
func (f *Fleet) GrantWarnings() []string {
	var out []string
	for _, name := range f.ComputerNames() {
		w := f.Computers[name].Power.WakeOnMessage
		if w == nil {
			continue
		}
		for _, pr := range w.From {
			kind, who, _ := strings.Cut(pr, ":")
			var send []string
			field := ""
			switch kind {
			case "computer":
				send, field = f.Computers[who].Grants.Send, "agent_computers."+who+".grants.send"
			case "service":
				send, field = f.Services[who].Grants.Send, "services."+who+".grants.send"
			default:
				continue
			}
			if !slices.Contains(send, name) {
				out = append(out, fmt.Sprintf("%s.power.wakeOnMessage.from: %s can't send to %s, so it never wakes it  fix: add %s to %s, or remove it", name, pr, name, name, field))
			}
		}
	}
	if len(f.Egress.AllowPrivate) > 0 {
		out = append(out, fmt.Sprintf("egress.allowPrivate %s: Services' named TCP egress may reach these private ranges", strings.Join(f.Egress.AllowPrivate, ", ")))
	}
	return out
}
