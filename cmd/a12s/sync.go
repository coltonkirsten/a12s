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

// inlineInstructionsMax is the longest instructions export leaves in fleet.yaml; longer ones become a preset file.
const inlineInstructionsMax = 2000

// runDiff compares the Fleet file with the live Fleet without changing anything: a dry run of a12s up.
func runDiff(g *globals, args []string) error {
	flags := g.flags("diff")
	pos, err := parse(flags, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("diff takes no arguments")
	}
	spec, f, err := g.loadSpec()
	if err != nil {
		return err
	}
	c, err := cli.Dial(f.Metadata.Name)
	if err != nil {
		return err
	}
	creds := api.Credentials{}
	if b, err := cli.LoadBindings(g.bindings()); err == nil {
		creds, _ = b.ResolveBound(cli.AllBound(b, f))
	}
	lock, err := fleet.LoadLock(g.lockPath())
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var dry api.PutFleetResponse
	req := api.PutFleetRequest{Fleet: spec, Lock: lock, Credentials: creds, File: true}
	if err := c.Do(ctx, "PUT", "/v1/fleet?dryRun=true", req, &dry); err != nil {
		return err
	}
	if g.json {
		if err := printJSON(dry.Changes); err != nil {
			return err
		}
	} else if len(dry.Changes) == 0 {
		fmt.Println("no differences")
	} else {
		fmt.Printf("%s -> live Fleet (a12s up applies these):\n", g.fleetPath)
		for _, line := range cli.ChangeLines(dry.Changes) {
			fmt.Println("  " + line)
		}
	}
	if len(dry.Changes) > 0 {
		return exitCode(2)
	}
	return nil
}

// runExport writes the live Fleet back to the Fleet file: short instructions stay inline, longer ones and files go
// to presets/<name>/, seccomp profiles to profiles/<name>.json. Credential values are never written.
func runExport(g *globals, args []string) error {
	flags := g.flags("export")
	force := flags.Bool("force", false, "overwrite files that differ from the export")
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
	all, err := exportFiles(&live.Fleet)
	if err != nil {
		return err
	}
	files := maps.Clone(all)
	dir := g.dir()
	var conflicts, wrote []string
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		old, err := os.ReadFile(filepath.Join(dir, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case bytes.Equal(old, files[rel]):
			delete(files, rel)
		default:
			conflicts = append(conflicts, rel)
		}
	}
	stale, err := stalePresetFiles(dir, all, &live.Fleet)
	if err != nil {
		return err
	}
	conflicts = append(conflicts, stale...)
	if len(conflicts) > 0 && !*force {
		return fmt.Errorf("these files differ from the live Fleet: %s\n  fix: commit or move them, then a12s export --force", strings.Join(conflicts, ", "))
	}
	for _, rel := range stale {
		if err := os.Remove(filepath.Join(dir, rel)); err != nil {
			return err
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
		return printJSON(map[string]any{"version": live.Version, "wrote": wrote, "removed": stale})
	}
	if len(wrote) == 0 && len(stale) == 0 {
		fmt.Printf("%s already matches the live Fleet (v%d)\n", g.fleetPath, live.Version)
		return nil
	}
	for _, rel := range wrote {
		fmt.Println("wrote " + rel)
	}
	for _, rel := range stale {
		fmt.Println("removed " + rel)
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
	var defaultsKind string
	one := func(dirName string, raw json.RawMessage, kind string) (*fleet.Computer, error) {
		var c fleet.Computer
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, err
			}
		}
		if c.Harness.Kind != "" {
			kind = c.Harness.Kind
		}
		if len(c.Files) > 0 || len(c.Instructions) > inlineInstructionsMax {
			preset := path.Join("presets", dirName)
			for p, body := range c.Files {
				out[path.Join(preset, p)] = []byte(body)
			}
			if c.Instructions != "" {
				out[path.Join(preset, fleet.InstructionsFile(kind))] = []byte(c.Instructions)
			}
			c.Files, c.Instructions, c.Home.Preset = nil, "", "./"+preset
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
		d, err := one("defaults", spec.Defaults, "")
		if err != nil {
			return nil, fmt.Errorf("defaults: %w", err)
		}
		defaultsKind = d.Harness.Kind
		_ = add("defaults", d)
	}
	if len(spec.Drives) > 0 {
		_ = add("drives", spec.Drives)
	}
	acs := yamlMap()
	for _, name := range spec.Names() {
		c, err := one(name, spec.AgentComputers[name], defaultsKind)
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

// stalePresetFiles are files in a preset directory export owns that the live Fleet does not have: left there, the
// next up would send them again.
func stalePresetFiles(dir string, all map[string][]byte, spec *fleet.Spec) ([]string, error) {
	var out []string
	names := append(spec.Names(), "defaults")
	for _, name := range names {
		root := filepath.Join(dir, "presets", name)
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			rel = filepath.ToSlash(rel)
			if _, ok := all[rel]; !ok {
				out = append(out, rel)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
