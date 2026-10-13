package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// runExport writes the live Fleet back to the Fleet file, instructions and files included, and seccomp profiles to
// profiles/<name>.json. Credential values are never written. It overwrites what differs: the live Fleet is the source
// of truth, and git diff shows what changed.
func runExport(g *globals, args []string) error {
	flags := g.flags("export")
	pos, err := parse(flags, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("export takes no arguments")
	}
	name, err := g.fleetName()
	if err != nil {
		return err
	}
	c, err := cli.Dial(name)
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var live api.FleetResponse
	if err := c.Do(ctx, "GET", "/v1/fleet", nil, &live); err != nil {
		return err
	}
	files, err := exportFiles(&live.Fleet)
	if err != nil {
		return err
	}
	dir := g.dir()
	var wrote []string
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		old, err := os.ReadFile(filepath.Join(dir, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case bytes.Equal(old, files[rel]):
			delete(files, rel)
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if bytes.HasPrefix(files[rel], []byte("#!")) {
			mode = 0o755
		}
		if err := os.WriteFile(p, files[rel], mode); err != nil {
			return err
		}
		wrote = append(wrote, rel)
	}
	if g.json {
		return printJSON(map[string]any{"version": live.Version, "wrote": wrote})
	}
	if len(wrote) == 0 {
		fmt.Printf("%s already matches the live Fleet (v%d)\n", g.fleetPath, live.Version)
		return nil
	}
	for _, rel := range wrote {
		fmt.Println("wrote " + rel)
	}
	return nil
}

func (g *globals) fleetName() (string, error) {
	f, err := g.readFleet()
	if err != nil {
		return "", err
	}
	return f.Metadata.Name, nil
}

// exportFiles is every file export writes, by path relative to the Fleet file's directory.
func exportFiles(spec *fleet.Spec) (map[string][]byte, error) {
	out := map[string][]byte{}
	one := func(dirName string, raw json.RawMessage) (*fleet.Computer, error) {
		var c fleet.Computer
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, err
			}
		}
		if prof := c.Security.Seccomp.Profile; len(prof) > 0 {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, prof, "", "  "); err != nil {
				return nil, err
			}
			p := path.Join("profiles", dirName+".json")
			out[p] = append(pretty.Bytes(), '\n')
			c.Security.Seccomp = fleet.Seccomp{Path: "./" + p}
		}
		return &c, nil
	}
	doc := yamlMap()
	add := func(k string, v any) error {
		n, err := yamlOf(v)
		if err != nil {
			return err
		}
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, n)
		return nil
	}
	if err := add("apiVersion", spec.APIVersion); err != nil {
		return nil, err
	}
	_ = add("kind", spec.Kind)
	_ = add("metadata", spec.Metadata)
	if spec.Egress.Enforcement != "" || len(spec.Egress.AllowPrivate) > 0 {
		_ = add("egress", spec.Egress)
	}
	if len(spec.Defaults) > 0 {
		d, err := one("defaults", spec.Defaults)
		if err != nil {
			return nil, fmt.Errorf("defaults: %w", err)
		}
		_ = add("defaults", d)
	}
	if len(spec.Drives) > 0 {
		_ = add("drives", spec.Drives)
	}
	acs := yamlMap()
	for _, name := range spec.Names() {
		c, err := one(name, spec.AgentComputers[name])
		if err != nil {
			return nil, fmt.Errorf("agent_computers.%s: %w", name, err)
		}
		n, err := yamlOf(c)
		if err != nil {
			return nil, err
		}
		acs.Content = append(acs.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, n)
	}
	doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "agent_computers"}, acs)
	if len(spec.Services) > 0 {
		_ = add("services", spec.Services)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	out["fleet.yaml"] = buf.Bytes()
	return out, nil
}

func yamlMap() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

// yamlOf goes through v's JSON, which keeps field order and an explicit empty list (an override of the defaults'
// list) where the YAML tags would drop it.
func yamlOf(v any) (*yaml.Node, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return yamlValue(dec)
}

func yamlValue(dec *json.Decoder) (*yaml.Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		if t == '{' {
			n = yamlMap()
		}
		for dec.More() {
			if t == '{' {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k.(string)})
			}
			child, err := yamlValue(dec)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, child)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		if len(n.Content) == 0 || n.Kind == yaml.SequenceNode && len(n.Content) <= 8 && allScalars(n.Content) {
			n.Style = yaml.FlowStyle
		}
		return n, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(t.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: t.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(t)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case string:
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: t}
		if strings.Contains(t, "\n") {
			n.Style = yaml.LiteralStyle
		}
		return n, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

func allScalars(ns []*yaml.Node) bool {
	for _, n := range ns {
		if n.Kind != yaml.ScalarNode {
			return false
		}
	}
	return true
}
