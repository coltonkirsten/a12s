package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

var errEmptyDocument = errors.New("empty document")

// rawFleet keeps defaults and agent computers as YAML nodes, so each decodes strictly on its own and scalars keep
// their exact text ("1.10" stays a string, not the float 1.1).
type rawFleet struct {
	APIVersion     string               `yaml:"apiVersion"`
	Kind           string               `yaml:"kind"`
	Metadata       Metadata             `yaml:"metadata"`
	Egress         FleetEgress          `yaml:"egress"`
	Defaults       yaml.Node            `yaml:"defaults"`
	Drives         map[string]Drive     `yaml:"drives"`
	AgentComputers map[string]yaml.Node `yaml:"agent_computers"`
	Computers      map[string]yaml.Node `yaml:"computers"`
	Services       map[string]yaml.Node `yaml:"services"`
}

func Load(path string) (*Fleet, error) {
	s, err := LoadSpec(path)
	if err != nil {
		return nil, err
	}
	f, err := s.Resolve()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// LoadSpec reads a Fleet file as written. home.preset and security.seccomp paths stay paths.
func LoadSpec(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet: %w", err)
	}
	s, err := DecodeSpec(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func Parse(data []byte) (*Fleet, error) {
	f, err := Decode(data)
	if err != nil {
		return nil, err
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f, nil
}

// Decode applies defaults without validating. Unknown fields are errors.
func Decode(data []byte) (*Fleet, error) {
	s, err := DecodeSpec(data)
	if err != nil {
		return nil, err
	}
	f, err := s.Effective()
	if err != nil {
		return nil, fmt.Errorf("parse fleet: %w", err)
	}
	return f, nil
}

// DecodeSpec decodes a Fleet file without merging defaults. It takes the old `computers` key for one release.
func DecodeSpec(data []byte) (*Spec, error) {
	var raw rawFleet
	if err := decodeStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("parse fleet: %w", err)
	}
	s := &Spec{
		APIVersion:     raw.APIVersion,
		Kind:           raw.Kind,
		Metadata:       raw.Metadata,
		Egress:         raw.Egress,
		Drives:         raw.Drives,
		AgentComputers: map[string]json.RawMessage{},
	}
	nodes := raw.AgentComputers
	if raw.Computers != nil {
		if raw.AgentComputers != nil {
			return nil, errors.New("parse fleet: both agent_computers and computers are set; rename computers to agent_computers")
		}
		nodes, s.LegacyKey = raw.Computers, true
	}
	var errs []error
	if err := computerOnlyEgress("defaults", &raw.Defaults); err != nil {
		errs = append(errs, err)
	}
	for _, name := range sortedKeys(nodes) {
		node := nodes[name]
		if err := computerOnlyEgress(name, &node); err != nil {
			errs = append(errs, err)
		}
	}
	if len(raw.Services) > 0 {
		s.Services = make(map[string]Service, len(raw.Services))
	}
	for _, name := range sortedKeys(raw.Services) {
		node := raw.Services[name]
		var svc Service
		if err := decodeService(name, &node, &svc); err != nil {
			errs = append(errs, err)
			continue
		}
		s.Services[name] = svc
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("parse fleet: %w", errors.Join(errs...))
	}
	var defaults Computer
	if err := decodeComputer(mapping(&raw.Defaults), &defaults); err != nil {
		return nil, fmt.Errorf("parse fleet: defaults: %w", err)
	}
	if d := SpecOf(defaults); string(d) != "{}" {
		s.Defaults = d
	}
	for _, name := range sortedKeys(nodes) {
		node := nodes[name]
		var c Computer
		if err := decodeComputer(mapping(&node), &c); err != nil {
			return nil, fmt.Errorf("parse fleet: agent_computers.%s: %w", name, err)
		}
		s.AgentComputers[name] = SpecOf(c)
	}
	return s, nil
}

// mapping returns other node kinds as is, so they fail to decode into a Computer.
func mapping(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	return n
}

func indexKey(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func decodeComputer(n *yaml.Node, c *Computer) error {
	if len(n.Content) == 0 && n.Kind == yaml.MappingNode {
		return nil
	}
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return decodeStrict(data, c)
}

func decodeStrict(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errEmptyDocument
		}
		return err
	}
	return nil
}
