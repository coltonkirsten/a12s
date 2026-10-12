package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

var errEmptyDocument = errors.New("empty document")

// rawFleet keeps defaults and computers as YAML nodes until defaults are merged
// in, so scalars keep their exact text ("1.10" stays a string, not the float 1.1).
type rawFleet struct {
	APIVersion string               `yaml:"apiVersion"`
	Kind       string               `yaml:"kind"`
	Metadata   Metadata             `yaml:"metadata"`
	Egress     FleetEgress          `yaml:"egress"`
	Defaults   yaml.Node            `yaml:"defaults"`
	Drives     map[string]Drive     `yaml:"drives"`
	Computers  map[string]yaml.Node `yaml:"computers"`
	Services   map[string]yaml.Node `yaml:"services"`
}

func Load(path string) (*Fleet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet: %w", err)
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
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
	var raw rawFleet
	if err := decodeStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("parse fleet: %w", err)
	}
	f := &Fleet{
		APIVersion: raw.APIVersion,
		Kind:       raw.Kind,
		Metadata:   raw.Metadata,
		Egress:     raw.Egress,
		Drives:     raw.Drives,
		Computers:  make(map[string]Computer, len(raw.Computers)),
	}
	var errs []error
	if err := computerOnlyEgress("defaults", &raw.Defaults); err != nil {
		errs = append(errs, err)
	}
	for _, name := range sortedKeys(raw.Computers) {
		node := raw.Computers[name]
		if err := computerOnlyEgress(name, &node); err != nil {
			errs = append(errs, err)
		}
	}
	if len(raw.Services) > 0 {
		f.Services = make(map[string]Service, len(raw.Services))
	}
	for _, name := range sortedKeys(raw.Services) {
		node := raw.Services[name]
		var s Service
		if err := decodeService(name, &node, &s); err != nil {
			errs = append(errs, err)
			continue
		}
		f.Services[name] = s
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("parse fleet: %w", errors.Join(errs...))
	}
	defaults := mapping(&raw.Defaults)
	if err := decodeComputer(defaults, &f.Defaults); err != nil {
		return nil, fmt.Errorf("parse fleet: defaults: %w", err)
	}
	for _, name := range sortedKeys(raw.Computers) {
		node := raw.Computers[name]
		var c Computer
		if err := decodeComputer(merge(defaults, mapping(&node)), &c); err != nil {
			return nil, fmt.Errorf("parse fleet: computers.%s: %w", name, err)
		}
		if c.Autonomy == "" {
			c.Autonomy = AutonomyFull
		}
		f.Computers[name] = c
	}
	return f, nil
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

// merge merges mappings key by key; scalars and lists replace. Neither input is modified.
func merge(base, over *yaml.Node) *yaml.Node {
	base, over = mapping(base), mapping(over)
	if base.Kind != yaml.MappingNode || over.Kind != yaml.MappingNode {
		return over
	}
	out := *base
	out.Content = append([]*yaml.Node(nil), base.Content...)
	for i := 0; i+1 < len(over.Content); i += 2 {
		key, val := over.Content[i], over.Content[i+1]
		j := indexKey(&out, key.Value)
		switch {
		case j < 0:
			out.Content = append(out.Content, key, val)
		case mapping(out.Content[j+1]).Kind == yaml.MappingNode && mapping(val).Kind == yaml.MappingNode && !isNull(val):
			out.Content[j+1] = merge(out.Content[j+1], val)
		default:
			out.Content[j+1] = val
		}
	}
	return &out
}

func indexKey(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }

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
