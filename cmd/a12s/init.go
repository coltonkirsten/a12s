package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// runInit is `a12s init [dir] [--harness claude-code|codex] [--with db,cron]`:
// it scaffolds fleet.yaml, a12s.bindings.yaml and .gitignore (enhancements
// §6.3). --with adds reference Services; a credential they need and nobody
// bound yet makes the first `a12s up` stop with its fix line
// (`a12s creds set postgres`), which is the only step left.
func runInit(g *globals, args []string) error {
	fs := g.flags("init")
	harness := fs.String("harness", "claude-code", "claude-code or codex")
	with := fs.String("with", "", "reference Services to add: db, cron (comma-separated)")
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
	var extras []string
	for _, w := range strings.Split(*with, ",") {
		switch w = strings.TrimSpace(w); w {
		case "":
		case "db", "cron":
			extras = append(extras, w)
		default:
			return usageError(fmt.Sprintf("--with %s: the reference Services are db and cron", w))
		}
	}
	text, bindings, err := scaffold(filepath.Base(absDir(dir)), *harness, extras)
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
	gi := filepath.Join(dir, ".gitignore")
	if data, err := os.ReadFile(gi); err != nil || !strings.Contains(string(data), "a12s.bindings.yaml") {
		f, err := os.OpenFile(gi, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		fmt.Fprintln(f, "# per-deployment credential bindings: never commit\na12s.bindings.yaml")
		f.Close()
	}
	fmt.Printf("wrote %s, a12s.bindings.yaml and .gitignore in %s\n", "fleet.yaml", dir)
	cred := "anthropic"
	if *harness == "codex" {
		cred = "openai"
	}
	fmt.Printf("next: a12s creds set %s", cred)
	for _, e := range extras {
		if e == "db" {
			fmt.Print(", a12s creds set postgres")
		}
	}
	fmt.Println(", then a12s up")
	return nil
}

var fs2NotExist = fs.ErrNotExist

func absDir(d string) string {
	if a, err := filepath.Abs(d); err == nil {
		return a
	}
	return d
}

// scaffold returns the Fleet file and the bindings file for init.
func scaffold(dirName, harness string, extras []string) (string, string, error) {
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
	db, cron := false, false
	for _, e := range extras {
		db, cron = db || e == "db", cron || e == "cron"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: %s }

computers:
  hello:
    harness: %s
    image: slim
    credentials: [%s]
    egress: { allow: [%s] }
`, name, h, cred, host)
	if db {
		b.WriteString("    # uses: [db]            # give hello the database tool (registered as a12s-db)\n")
	}
	if db || cron {
		b.WriteString("\nservices:\n")
	}
	if db {
		b.WriteString(`  pg:                       # the Fleet's own Postgres, a stock image described inline
    image: postgres:17
    manifest:
      ports: { db: { port: 5432, protocol: tcp } }
      health: { exec: ["pg_isready", "-U", "postgres"] }
      state: { at: /var/lib/postgresql/data, size: 5Gi }
      tmpfs: [/var/run/postgresql]
      user: 999
    credentials: [postgres]
    config: { env: { POSTGRES_PASSWORD_FILE: /run/a12s/secrets/postgres } }

  db:                       # an MCP server that holds the database login; Computers that use it never see it
    image: a12s/svc-mcp-postgres:0.1
    credentials: [postgres]
    uses: [pg]
    config:
      command: ["/opt/mcp/bin/mcp-server-postgres"]
      args: ["postgresql://postgres@${PGHOST}:${PGPORT}/postgres"]
      envFrom: { PGPASSWORD: postgres }
      hostPortFrom: { A12S_SERVICE_PG_DB: [PGHOST, PGPORT] }
`)
	}
	if cron {
		b.WriteString(`  clock:                    # a cron: enqueues into hello's inbox on a schedule
    image: a12s/svc-cron:0.1
    grants: { send: [hello] }
    config:
      jobs:
        - { name: morning, cron: "0 7 * * *", to: hello, body: "Good morning. Summarize what is on your plate." }
`)
	}
	bindings := "# Per deployment, git-ignored. Maps each credential name in fleet.yaml to where it comes from here.\n# a12s creds set <name> fills this in.\ncredentials: {}\n"
	return b.String(), bindings, nil
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
