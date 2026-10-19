package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/coltonkirsten/a12s"
)

// runInit writes a Fleet file plus a README, llms.txt and agent instructions; the one step left is `a12s creds set`.
func runInit(g *globals, args []string) error {
	flags := g.flags("init")
	harness := flags.String("harness", "claude-code", "claude-code or codex")
	pos, err := parse(flags, args)
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
	text, err := scaffold(filepath.Base(absDir(dir)), *harness)
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
	cred := "anthropic"
	if *harness == "codex" {
		cred = "openai"
	}
	files := []string{fleetPath}
	for _, f := range []struct{ name, text string }{
		{"README.md", starterReadme(filepath.Base(absDir(dir)), cred)},
		{"llms.txt", a12s.LLMsTxt},
		{"AGENTS.md", starterAgents},
		{"CLAUDE.md", "@AGENTS.md\n"},
	} {
		p := filepath.Join(dir, f.name)
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.WriteFile(p, []byte(f.text), 0o644); err != nil {
			return err
		}
		files = append(files, p)
	}
	if g.json {
		return printJSON(map[string]any{"dir": dir, "files": files})
	}
	names := make([]string, len(files))
	for i, p := range files {
		names[i] = filepath.Base(p)
	}
	fmt.Printf("wrote %s in %s\n", strings.Join(names, ", "), dir)
	fmt.Printf("next: a12s creds set %s, then a12s up\n", cred)
	return nil
}

func absDir(d string) string {
	if a, err := filepath.Abs(d); err == nil {
		return a
	}
	return d
}

func scaffold(dirName, harness string) (string, error) {
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
		return "", usageError("--harness is claude-code or codex")
	}
	text := fmt.Sprintf(`apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: %s }

defaults:                      # every agent computer gets these unless it sets its own
  harness: %s
  image: slim
  credentials: [%s]
  egress: { allow: [%s] }

agent_computers:
  assistant:
    instructions: You are a helpful assistant. Keep answers short.
`, name, h, cred, host)
	return text, nil
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

func starterReadme(name, cred string) string {
	return "# " + name + "\n\n" +
		"An a12s Fleet. a12s is installed like Docker: never vendor or edit its source here.\n" +
		"Extend it through fleet.yaml, your own images and Services, and the client SDK.\n\n" +
		"```sh\na12s creds set " + cred + "   # per Fleet, and again after a12s down\na12s up\na12s send assistant \"hi\" --wait\na12s down\n```\n\n" +
		"After `a12s up`, create agent computers from code in this folder (`pip install a12s` or\n" +
		"`npm install @coltonkirsten/a12s`). Each one gets the defaults in fleet.yaml.\n\n" +
		"```python\n" + helloPython + "```\n\n" +
		"```js\n" + helloJS + "```\n\n" +
		"Everything else is in ./llms.txt, the whole a12s surface for the a12s version that wrote it.\n" +
		"The latest is at https://raw.githubusercontent.com/coltonkirsten/a12s/main/llms.txt\n"
}

const starterAgents = `# Agent guidelines

This folder is an a12s Fleet. Before writing code against a12s, read ./llms.txt: it is the whole user-facing
surface (CLI, fleet.yaml, SDKs) for the installed a12s version. a12s is installed like Docker: never vendor or edit
its source here; extend it through fleet.yaml, your own images and Services, and the client SDK.
`

// The repo README's hello example, verbatim; TestHelloMatchesReadme keeps the two the same.
const helloPython = `import a12s

fleet = a12s.Fleet()            # connects to the running fleet (or url=, token=)
hello = fleet.agent_computers.create("hello", instructions="Be helpful")
print(hello.send("hi").wait().text)
hello.remove()
`

const helloJS = `import { Fleet } from "@coltonkirsten/a12s";

const fleet = await Fleet.connect();
const hello = await fleet.agentComputers.create("hello", { instructions: "Be helpful" });
console.log((await hello.send("hi").wait()).text);
await hello.remove();
`
