package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ManifestPath is where an a12s Service image carries its manifest.
const ManifestPath = "/a12s/service.json"

// Service manifest defaults (§5.6).
const (
	DefaultHealthInterval = 10 * time.Second
	DefaultHealthStartup  = 60 * time.Second
)

// ServiceManifest is /a12s/service.json in a Service image, or the inline
// services.<name>.manifest (§5.6). Every field is optional here; the merged
// result is validated by Validate.
type ServiceManifest struct {
	A12S        int                    `yaml:"a12s,omitempty" json:"a12s,omitempty"`
	Name        string                 `yaml:"name,omitempty" json:"name,omitempty"`
	Version     string                 `yaml:"version,omitempty" json:"version,omitempty"`
	User        *ServiceUser           `yaml:"user,omitempty" json:"user,omitempty"`
	Health      *ServiceHealth         `yaml:"health,omitempty" json:"health,omitempty"`
	Ports       map[string]ServicePort `yaml:"ports,omitempty" json:"ports,omitempty"`
	Credentials []ManifestCredential   `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	Login       *ServiceLogin          `yaml:"login,omitempty" json:"login,omitempty"`
	State       *ManifestState         `yaml:"state,omitempty" json:"state,omitempty"`
	Tmpfs       []string               `yaml:"tmpfs,omitempty" json:"tmpfs,omitempty"`
	Resources   *Resources             `yaml:"resources,omitempty" json:"resources,omitempty"`
}

// ServiceUser is the uid (and gid) the image's process runs as: a number,
// or "uid:gid". Never 0.
type ServiceUser struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// String is "uid:gid".
func (u ServiceUser) String() string { return strconv.Itoa(u.UID) + ":" + strconv.Itoa(u.GID) }

func parseUser(s string) (ServiceUser, error) {
	us, gs, hasGID := strings.Cut(strings.TrimSpace(s), ":")
	uid, err := strconv.Atoi(us)
	if err != nil {
		return ServiceUser{}, fmt.Errorf("user %q: want a uid or \"uid:gid\"", s)
	}
	gid := uid
	if hasGID {
		if gid, err = strconv.Atoi(gs); err != nil {
			return ServiceUser{}, fmt.Errorf("user %q: want a uid or \"uid:gid\"", s)
		}
	}
	return ServiceUser{UID: uid, GID: gid}, nil
}

// UnmarshalYAML accepts 999 or "999:999".
func (u *ServiceUser) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseUser(n.Value)
	if err != nil {
		return err
	}
	*u = v
	return nil
}

// MarshalYAML writes "uid:gid".
func (u ServiceUser) MarshalYAML() (any, error) { return u.String(), nil }

// UnmarshalJSON accepts 999, "999" or "999:999".
func (u *ServiceUser) UnmarshalJSON(b []byte) error {
	var n json.Number
	if json.Unmarshal(b, &n) == nil {
		v, err := parseUser(n.String())
		if err != nil {
			return err
		}
		*u = v
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("user: want a uid or \"uid:gid\"")
	}
	v, err := parseUser(s)
	if err != nil {
		return err
	}
	*u = v
	return nil
}

// MarshalJSON writes "uid:gid".
func (u ServiceUser) MarshalJSON() ([]byte, error) { return json.Marshal(u.String()) }

// ServiceHealth is the health check: exactly one of HTTP and Exec.
type ServiceHealth struct {
	HTTP     *HealthHTTP `yaml:"http,omitempty" json:"http,omitempty"`
	Exec     []string    `yaml:"exec,omitempty" json:"exec,omitempty"`
	Interval string      `yaml:"interval,omitempty" json:"interval,omitempty"`
	Startup  string      `yaml:"startup,omitempty" json:"startup,omitempty"`
}

// HealthHTTP is an HTTP health check: a 2xx or 3xx answer passes.
type HealthHTTP struct {
	Port int    `yaml:"port" json:"port"`
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
}

// IntervalDuration is the check interval (default 10s).
func (h ServiceHealth) IntervalDuration() time.Duration {
	if d, err := time.ParseDuration(h.Interval); err == nil && d > 0 {
		return d
	}
	return DefaultHealthInterval
}

// StartupDuration is the startup grace (default 60s).
func (h ServiceHealth) StartupDuration() time.Duration {
	if d, err := time.ParseDuration(h.Startup); err == nil && d > 0 {
		return d
	}
	return DefaultHealthStartup
}

// Service port protocols.
const (
	ProtocolHTTP = "http"
	ProtocolTCP  = "tcp"
)

// ServicePort is one declared port: the only ports the proxy routes to.
type ServicePort struct {
	Port     int      `yaml:"port" json:"port"`
	Protocol string   `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	MCP      *PortMCP `yaml:"mcp,omitempty" json:"mcp,omitempty"`
}

// Proto is the port's protocol, http by default.
func (p ServicePort) Proto() string {
	if p.Protocol == "" {
		return ProtocolHTTP
	}
	return p.Protocol
}

// PortMCP marks an HTTP port as an MCP endpoint at Path.
type PortMCP struct {
	Path string `yaml:"path" json:"path"`
}

// ManifestCredential is a credential the image expects.
type ManifestCredential struct {
	Name     string `yaml:"name" json:"name"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// ServiceLogin is the interactive sign-in `a12s service login` runs.
type ServiceLogin struct {
	Command  []string `yaml:"command" json:"command"`
	Produces string   `yaml:"produces" json:"produces"`
}

// ManifestState asks for a persistent, exclusive drive mounted at At.
type ManifestState struct {
	At   string `yaml:"at" json:"at"`
	Size Bytes  `yaml:"size,omitempty" json:"size,omitzero"`
}

// DefaultStateSize is a state drive's size when nothing says otherwise.
const DefaultStateSize = Bytes(1 << 30)

// Merge returns the image manifest with the inline manifest's fields laid
// over it, field by field (inline fields override the image's). Either may
// be nil; the result is nil only when both are.
func MergeManifest(image, inline *ServiceManifest) *ServiceManifest {
	if image == nil && inline == nil {
		return nil
	}
	out := ServiceManifest{}
	if image != nil {
		out = *image
	}
	if inline == nil {
		return &out
	}
	in := *inline
	if in.A12S != 0 {
		out.A12S = in.A12S
	}
	if in.Name != "" {
		out.Name = in.Name
	}
	if in.Version != "" {
		out.Version = in.Version
	}
	if in.User != nil {
		out.User = in.User
	}
	if in.Health != nil {
		out.Health = in.Health
	}
	if in.Ports != nil {
		out.Ports = in.Ports
	}
	if in.Credentials != nil {
		out.Credentials = in.Credentials
	}
	if in.Login != nil {
		out.Login = in.Login
	}
	if in.State != nil {
		out.State = in.State
	}
	if in.Tmpfs != nil {
		out.Tmpfs = in.Tmpfs
	}
	if in.Resources != nil {
		out.Resources = in.Resources
	}
	return &out
}

// ParseManifest decodes service.json strictly.
func ParseManifest(data []byte) (*ServiceManifest, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var m ServiceManifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

var portNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// validatePartial checks what an inline manifest says on its own (the
// merged manifest is checked again in full at admission).
func (m *ServiceManifest) validatePartial(field string) []error {
	var errs []error
	bad := func(sub, fix, format string, args ...any) {
		msg := fmt.Sprintf("%s.%s: %s", field, sub, fmt.Sprintf(format, args...))
		if fix != "" {
			msg += "  fix: " + fix
		}
		errs = append(errs, errors.New(msg))
	}
	if m.User != nil && (m.User.UID <= 0 || m.User.GID < 0) {
		bad("user", "run the image as a non-root uid", "%s: the image's process never runs as root", m.User)
	}
	if h := m.Health; h != nil {
		if (h.HTTP == nil) == (len(h.Exec) == 0) {
			bad("health", "set health.http {port, path} or health.exec [command...]", "needs exactly one of http and exec")
		}
		if h.HTTP != nil && (h.HTTP.Port < 1 || h.HTTP.Port > 65535) {
			bad("health.http.port", "", "%d must be 1 to 65535", h.HTTP.Port)
		}
		if h.HTTP != nil && h.HTTP.Path != "" && !strings.HasPrefix(h.HTTP.Path, "/") {
			bad("health.http.path", "", "%q must start with /", h.HTTP.Path)
		}
		for _, d := range []struct{ name, v string }{{"interval", h.Interval}, {"startup", h.Startup}} {
			if d.v == "" {
				continue
			}
			if v, err := time.ParseDuration(d.v); err != nil || v <= 0 {
				bad("health."+d.name, "use a duration such as 10s", "%q is not a duration", d.v)
			}
		}
	}
	for _, name := range sortedKeys(m.Ports) {
		p := m.Ports[name]
		if !portNameRE.MatchString(name) {
			bad("ports."+name, "", "port names are lowercase DNS labels of at most 32 characters")
		}
		if p.Port < 1 || p.Port > 65535 {
			bad("ports."+name+".port", "", "%d must be 1 to 65535", p.Port)
		}
		switch p.Proto() {
		case ProtocolHTTP:
		case ProtocolTCP:
			if p.MCP != nil {
				bad("ports."+name+".mcp", "use protocol http for an MCP port", "an MCP endpoint is HTTP")
			}
		default:
			bad("ports."+name+".protocol", "", "must be http or tcp, got %q", p.Protocol)
		}
		if p.MCP != nil && !strings.HasPrefix(p.MCP.Path, "/") {
			bad("ports."+name+".mcp.path", "", "%q must start with /", p.MCP.Path)
		}
	}
	seen := map[string]bool{}
	for _, c := range m.Credentials {
		if !credentialRE.MatchString(c.Name) {
			bad("credentials", "", "%q is not a valid credential name", c.Name)
		}
		if seen[c.Name] {
			bad("credentials", "", "%q is listed twice", c.Name)
		}
		seen[c.Name] = true
	}
	if l := m.Login; l != nil {
		if len(l.Command) == 0 {
			bad("login.command", "", "is required")
		}
		if !credentialRE.MatchString(l.Produces) {
			bad("login.produces", "", "%q is not a valid credential name", l.Produces)
		}
	}
	if st := m.State; st != nil {
		if err := checkServicePath(st.At); err != nil {
			bad("state.at", "", "%v", err)
		}
	}
	for _, t := range m.Tmpfs {
		if err := checkServicePath(t); err != nil {
			bad("tmpfs", "", "%v", err)
		}
	}
	if m.State != nil && slices.Contains(m.Tmpfs, m.State.At) {
		bad("tmpfs", "", "%q is also the state drive", m.State.At)
	}
	return errs
}

// ValidateService checks the merged manifest of services.<name> against
// the Service's Fleet entry and the image's declared volumes (§5.6, §6.2).
// m may be nil: the image is then not an a12s Service.
func ValidateService(name string, s Service, m *ServiceManifest, image string, volumes []string) error {
	field := "services." + name
	if m == nil {
		return fmt.Errorf("image %s is not an a12s Service (no %s)  fix: add %s.manifest or see docs/services.md#manifest", image, ManifestPath, field)
	}
	errs := m.validatePartial(field + ".manifest")
	bad := func(sub, fix, format string, args ...any) {
		msg := fmt.Sprintf("%s.%s: %s", field, sub, fmt.Sprintf(format, args...))
		if fix != "" {
			msg += "  fix: " + fix
		}
		errs = append(errs, errors.New(msg))
	}
	if m.A12S != 0 && m.A12S != 1 {
		bad("manifest.a12s", "", "manifest version %d is not supported (want 1)", m.A12S)
	}
	if m.User == nil {
		bad("manifest.user", "set manifest.user to the uid the image runs as (never 0)", "is required")
	}
	if m.Health == nil {
		bad("manifest.health", "set health.http {port, path} or health.exec [command...]", "is required")
	}
	declared := map[int]bool{}
	for _, p := range m.Ports {
		declared[p.Port] = true
	}
	for _, p := range s.Publish {
		pub, err := ParsePublish(p)
		if err == nil && !declared[pub.Port] {
			bad("publish", "declare the port in the manifest's ports, or remove it", "port %d is not a declared port of %s", pub.Port, name)
		}
	}
	have := map[string]bool{}
	for _, c := range s.Credentials {
		have[c] = true
	}
	for _, c := range m.Credentials {
		if c.Required && !have[c.Name] {
			bad("credentials", "add "+c.Name+" to "+field+".credentials", "the image requires credential %s", c.Name)
		}
	}
	if m.Login != nil && !have[m.Login.Produces] {
		bad("credentials", "add "+m.Login.Produces+" to "+field+".credentials", "the manifest's login produces %s, which the Service does not list", m.Login.Produces)
	}
	for _, v := range volumes {
		v = path.Clean(v)
		if !coveredBy(v, m) {
			bad("manifest", "set manifest.state.at to "+v+", or list it in manifest.tmpfs", "the image declares VOLUME %s, which neither state nor tmpfs covers", v)
		}
	}
	return errors.Join(errs...)
}

// coveredBy reports whether an image VOLUME is provided by a12s: it is the
// state drive's mount point, or a tmpfs path or under one. (A VOLUME below
// the state mount would get an anonymous Docker volume of its own, which
// a12s neither tracks nor removes, so it is not covered.)
func coveredBy(v string, m *ServiceManifest) bool {
	under := func(p string) bool { return v == p || strings.HasPrefix(v, p+"/") }
	if m.State != nil && v == m.State.At {
		return true
	}
	for _, t := range m.Tmpfs {
		if under(t) {
			return true
		}
	}
	return false
}

// MCPPorts returns the manifest's MCP ports by name, sorted.
func (m *ServiceManifest) MCPPorts() []string {
	var out []string
	for _, name := range sortedKeys(m.Ports) {
		if m.Ports[name].MCP != nil {
			out = append(out, name)
		}
	}
	return out
}
