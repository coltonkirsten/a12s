package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CredentialPath is where `a12s creds set` and `a12s service login` keep a
// credential on the host: ~/.a12s/<fleet>/<name>.
func CredentialPath(fleetName, name string) (string, error) {
	dir, err := StateDir(fleetName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// WriteSecret writes a credential atomically: a 0600 temporary file in the
// same directory, renamed over path (enhancements §5.5 step 4).
func WriteSecret(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// BindFile records credentials.<name>: {file: <file>} in the bindings file,
// creating it (0600) if needed and keeping everything else in it.
func BindFile(bindingsPath, name, file string) error {
	data, err := os.ReadFile(bindingsPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var doc yaml.Node
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", bindingsPath, err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: not a mapping", bindingsPath)
	}
	creds := child(root, "credentials")
	if creds == nil {
		creds = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "credentials"}, creds)
	}
	if creds.Kind != yaml.MappingNode {
		creds.Kind, creds.Tag, creds.Content, creds.Value = yaml.MappingNode, "!!map", nil, ""
	}
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "file"}, {Kind: yaml.ScalarNode, Value: homeRelative(file)},
	}}
	replaced := false
	for i := 0; i+1 < len(creds.Content); i += 2 {
		if creds.Content[i].Value == name {
			creds.Content[i+1], replaced = entry, true
		}
	}
	if !replaced {
		creds.Content = append(creds.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, entry)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	return WriteSecret(bindingsPath, buf.Bytes())
}

func child(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// homeRelative writes a path under the home directory as ~/….
func homeRelative(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~/" + strings.TrimPrefix(p, home+string(filepath.Separator))
	}
	return p
}
