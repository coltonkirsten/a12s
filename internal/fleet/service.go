package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
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
}

type ServiceEgress struct {
	Allow []string `yaml:"allow" json:"allow"`
}

func (e ServiceEgress) AllowsAll() bool { return Egress{Allow: e.Allow}.AllowsAll() }

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
	{"harness", "a Service runs no harness", "remove it; a Service that wants a harness is an agent computer"},
	{"inbox", "a Service has no inbox", "remove it"},
	{"power", "a Service has no power policy (no auto-sleep, no wake on message)", "remove it; a12s sleep and a12s wake still work on Services"},
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

var reservedFn = map[string]bool{"controller": true}

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
			bad(field, "rename one of them", "%s is already an agent computer; agent computers and Services share one <name>.a12s namespace", name)
		}
		switch {
		case s.Image == "":
			bad(field+".image", "set an image reference", "required")
		case s.Image == TierSlim:
			bad(field+".image", "use the Service's own image reference", "%q is the agent computer base image; a Service runs its own image", s.Image)
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
		errs = append(errs, f.validateGrants(field, s.Grants)...)
		errs = append(errs, f.validateUses(field, name, s.Uses)...)
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
			errs = append(errs, fmt.Errorf("%s.grants.send: %q: there is no wildcard  fix: name each agent computer", field, to))
		case f.hasService(to):
			errs = append(errs, fmt.Errorf("%s.grants.send: %s is a Service, and only agent computers have inboxes  fix: remove it from %s.grants.send", field, to, field))
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
				errs = append(errs, fmt.Errorf("%s.uses: %s is an agent computer; uses reaches Services only, and the inbox is the only way to an agent computer  fix: remove it, or add it to %s.grants.send", field, u.Name, field))
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

// GrantWarnings: a principal in a power policy that can't send to the
// Computer never wakes it.
func (f *Fleet) GrantWarnings() []string {
	var out []string
	// A grant may name an agent computer that doesn't exist yet, so two that send to each other can be created one at
	// a time.
	dangling := func(field string, send []string) {
		for _, to := range send {
			if _, ok := f.Computers[to]; !ok && !f.hasService(to) && !strings.Contains(to, "*") {
				out = append(out, fmt.Sprintf("%s.grants.send: there is no agent computer %s (yet); sends to it fail until it exists", field, to))
			}
		}
	}
	for _, name := range f.ComputerNames() {
		dangling("agent_computers."+name, f.Computers[name].Grants.Send)
	}
	for _, name := range f.ServiceNames() {
		dangling("services."+name, f.Services[name].Grants.Send)
	}
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
	return out
}
