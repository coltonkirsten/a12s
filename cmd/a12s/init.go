package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/version"
)

// runInit leaves credentials unbound on purpose: the first `a12s up` stops with
// the fix line (`a12s creds set anthropic`), the only step left.
func runInit(g *globals, args []string) error {
	fs := g.flags("init")
	harness := fs.String("harness", "claude-code", "claude-code or codex")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageError("init takes at most one directory")
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	text, bindings, err := scaffold(filepath.Base(absDir(dir)), *harness)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fleetPath := filepath.Join(dir, "fleet.yaml")
	if _, err := os.Stat(fleetPath); err == nil {
		return fmt.Errorf("%s already exists; init never overwrites a Fleet file", fleetPath)
	}
	if err := os.WriteFile(fleetPath, []byte(text), 0o644); err != nil {
		return err
	}
	bpath := filepath.Join(dir, "a12s.bindings.yaml")
	if _, err := os.Stat(bpath); errors.Is(err, fs2NotExist) {
		if err := os.WriteFile(bpath, []byte(bindings), 0o600); err != nil {
			return err
		}
	}
	lockPath := filepath.Join(dir, fleet.LockFile)
	if _, err := os.Stat(lockPath); errors.Is(err, fs2NotExist) {
		l := fleet.NewLock()
		if version.Release() {
			l.A12s = version.Version
		}
		if err := l.Save(lockPath); err != nil {
			return err
		}
	}
	readme := filepath.Join(dir, "README.md")
	if _, err := os.Stat(readme); errors.Is(err, fs2NotExist) {
		if err := os.WriteFile(readme, []byte(starterReadme(filepath.Base(absDir(dir)))), 0o644); err != nil {
			return err
		}
	}
	gi := filepath.Join(dir, ".gitignore")
	if data, err := os.ReadFile(gi); err != nil || !strings.Contains(string(data), "a12s.bindings.yaml") {
		f, err := os.OpenFile(gi, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		fmt.Fprintln(f, "# per-deployment credential bindings: never commit\na12s.bindings.yaml")
		f.Close()
	}
	if g.json {
		return printJSON(map[string]any{"dir": dir, "files": []string{fleetPath, lockPath, bpath, gi, readme}})
	}
	fmt.Printf("wrote fleet.yaml, fleet.lock, a12s.bindings.yaml, .gitignore and README.md in %s\n", dir)
	cred := "anthropic"
	if *harness == "codex" {
		cred = "openai"
	}
	fmt.Printf("next: a12s creds set %s, then a12s up\n", cred)
	return nil
}

var fs2NotExist = fs.ErrNotExist

func absDir(d string) string {
	if a, err := filepath.Abs(d); err == nil {
		return a
	}
	return d
}

func scaffold(dirName, harness string) (string, string, error) {
	name := strings.ToLower(dirName)
	if !validFleetName(name) {
		name = "fleet"
	}
	var h, cred, host string
	switch harness {
	case "claude-code":
		h, cred, host = "{ kind: claude-code, version: 2.1.280 }", "anthropic", "api.anthropic.com"
	case "codex":
		h, cred, host = "{ kind: codex, version: 0.154.0 }", "openai", "api.openai.com"
	default:
		return "", "", usageError("--harness is claude-code or codex")
	}
	text := fmt.Sprintf(`apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: %s }

computers:
  hello:
    harness: %s
    image: slim
    credentials: [%s]
    egress: { allow: [%s] }
`, name, h, cred, host)
	bindings := "# Per deployment, git-ignored. Maps each credential name in fleet.yaml to where it comes from here.\n# a12s creds set <name> fills this in.\ncredentials: {}\n"
	return text, bindings, nil
}

func validFleetName(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(s)-1) {
			return false
		}
	}
	return true
}

func starterReadme(name string) string {
	pin := "the a12s version in fleet.lock"
	if version.Release() {
		pin = "a12s " + version.Version + ", pinned in fleet.lock"
	}
	return "# " + name + "\n\n" +
		"An a12s Fleet. a12s is a pinned dependency (" + pin + "), installed like Docker: never vendor or edit its source here.\n" +
		"Extend it through fleet.yaml, your own images and Services, and the client SDK.\n\n" +
		"```sh\na12s creds set anthropic   # once per machine\na12s up\na12s prompt hello \"hi\"\na12s down\n```\n"
}
