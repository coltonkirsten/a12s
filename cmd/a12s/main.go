// Command a12s is the a12s CLI and, as `a12s controller`, the Controller.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

type command struct {
	name    string
	args    string
	summary string
	run     func(g *globals, args []string) error
}

func commands() []command {
	return []command{
		{"init", "[dir] [--harness claude-code|codex]", "scaffold fleet.yaml, bindings and .gitignore", runInit},
		{"up", "[--asleep] [--force] [--memory-only-credentials]", "apply the Fleet file: create, update or stage agent computers", runUp},
		{"diff", "", "the Fleet file against the live Fleet; exit 2 when they differ", runDiff},
		{"export", "[--force]", "write the live Fleet back to the Fleet file and presets", runExport},
		{"apply", "--interrupt [--grace 30s]", "force a staged change onto a busy Computer", runApply},
		{"status", "[<name>]", "state, session, active turn, harness version, warnings", runStatus},
		{"prompt", "<name> [--new] \"…\"", "send a turn and stream its events", runPrompt},
		{"send", "<name> \"…\" [--fresh] [--expires d] [--replace k] [--meta k=v]", "enqueue a message and exit (works while asleep)", runSend},
		{"inbox", "<name> [--all] [--bodies] | rm|clear|hold|resume <name>", "list or manage a Computer's inbox", runInbox},
		{"token", "create <name> --prefix p | --actions a,b --agent-computers c | ls | rm <name>", "API tokens for other programs", runToken},
		{"stop", "<name> [--hold]", "interrupt the active turn (--hold: hold the inbox first)", runStop},
		{"logs", "<name> [--follow] [--since <seq>]", "replay and follow the journal", runLogs},
		{"sleep", "<name>... | --all [--allow-wake]", "sleep Computers (storage kept, no CPU or RAM)", runSleep},
		{"wake", "<name>... | --all [--clear-inbox]", "wake Computers; resume happens on the next prompt", runWake},
		{"gateway", "[--web dir | --dev url | -- npm run dev] [--port 7012] [--allow actions]", "serve a web app (or the built-in dashboard) that can reach the Fleet", runGateway},
		{"ui", "[console args, e.g. --port 7013 --no-open]", "open the web console (a12s-console, via npx; Node.js 20+)", runUI},
		{"shell", "<name> [--root] [-- command...]", "audited terminal inside a Computer, outside the harness", runShell},
		{"service", "ls | status <svc> | logs <svc> [-f] | login <svc>", "list Services, show one, follow its logs, or log it in once", runService},
		{"creds", "set <name>", "paste a token once; it is stored 0600 and bound", runCreds},
		{"lock", "update services/<svc>", "move a Service's pin in fleet.lock explicitly", runLock},
		{"drive", "ls | rm <name> [--yes]", "list drives; rm and down --purge are the only commands that destroy data", runDrive},
		{"down", "[--purge [--yes]]", "remove Computers, Controller and network; keep drives (--purge destroys them)", runDown},
		{"doctor", "", "check Docker, images, bindings, the Controller and Service health; one fix per line", runDoctor},
		{"version", "", "print the a12s release and where its images come from", runVersion},
		{"completion", "bash|zsh", "print a shell completion script", runCompletion},
		{"controller", "", "run the Controller (inside its container)", runControllerCmd},
	}
}

type globals struct {
	fleetPath    string
	bindingsPath string
	json         bool
	warnedF      bool
}

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		args = []string{"version"}
	}
	g := &globals{fleetPath: defaultFleetPath()}
	fs := flag.NewFlagSet("a12s", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return 0
		}
		fmt.Fprintf(stderr, "a12s: %v\n", err)
		usage(stderr)
		return 2
	}
	rest := fs.Args()
	if len(rest) > 0 {
		if err, ok := hidden(rest[0], rest[1:]); ok {
			if err != nil {
				fmt.Fprintf(stderr, "a12s %s: %v\n", rest[0], err)
				return 1
			}
			return 0
		}
	}
	if len(rest) == 0 || rest[0] == "help" {
		usage(stderr)
		return 2
	}
	for _, c := range commands() {
		if c.name == rest[0] {
			return report(stderr, c, c.run(g, rest[1:]))
		}
	}
	fmt.Fprintf(stderr, "a12s: unknown command %q\n", rest[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: a12s [--fleet|-F fleet.yaml] [--bindings path] [--json] <command> [args]")
	fmt.Fprintln(w)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %-38s %s\n", c.name, c.args, c.summary)
	}
}

// usageError exits with code 2.
type usageError string

func (e usageError) Error() string { return string(e) }

// exitCode prints no message: the command has already said what happened.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func report(w io.Writer, c command, err error) int {
	var code exitCode
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(os.Stdout, "usage: a12s %s %s\n  %s\n", c.name, c.args, c.summary)
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, &ue):
		fmt.Fprintf(w, "a12s: %s\nusage: a12s %s %s\n", ue, c.name, c.args)
		return 2
	case errors.Is(err, context.Canceled):
		return 130
	}
	fmt.Fprintf(w, "a12s: %v\n", err)
	return 1
}

func (g *globals) register(fs *flag.FlagSet) {
	g.registerFleet(fs, true)
}

// registerFleet: commands where -f means follow pass withF false.
func (g *globals) registerFleet(fs *flag.FlagSet, withF bool) {
	usage := "Fleet file; fleet.lock and " + cli.BindingsFile + " live beside it (default: $A12S_FLEET, then the nearest fleet.yaml upward)"
	fs.StringVar(&g.fleetPath, "fleet", g.fleetPath, usage)
	fs.StringVar(&g.fleetPath, "F", g.fleetPath, usage)
	if withF {
		fs.Func("f", "deprecated: use --fleet or -F", func(v string) error {
			g.fleetPath = v
			if !g.warnedF {
				g.warnedF = true
				fmt.Fprintln(os.Stderr, "a12s: -f for the Fleet file is deprecated and goes away in the next release; use --fleet or -F (-f will mean follow)")
			}
			return nil
		})
	}
	fs.StringVar(&g.bindingsPath, "bindings", g.bindingsPath, "bindings file (default: "+cli.BindingsFile+" beside the Fleet file)")
	fs.BoolVar(&g.json, "json", g.json, "print the result as JSON (events as JSON lines)")
}

func (g *globals) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	return fs
}

func (g *globals) flagsFollow(name string, follow *bool) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.registerFleet(fs, false)
	fs.BoolVar(follow, "f", false, "follow")
	fs.BoolVar(follow, "follow", false, "follow")
	return fs
}

// defaultFleetPath is $A12S_FLEET, else the nearest fleet.yaml from the
// working directory upward, else fleet.yaml.
func defaultFleetPath() string {
	if p := os.Getenv("A12S_FLEET"); p != "" && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") || strings.Contains(p, string(filepath.Separator))) {
		return p
	}
	dir, err := os.Getwd()
	if err != nil {
		return "fleet.yaml"
	}
	for {
		p := filepath.Join(dir, "fleet.yaml")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			if rel, err := filepath.Rel(mustGetwd(), p); err == nil {
				return rel
			}
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "fleet.yaml"
		}
		dir = parent
	}
}

func mustGetwd() string {
	d, _ := os.Getwd()
	return d
}

// parse accepts flags anywhere among the arguments, unlike flag.Parse.
// Everything after "--" is positional.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError(err.Error())
		}
		rest := fs.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (g *globals) dir() string { return filepath.Dir(g.fleetPath) }

func (g *globals) lockPath() string { return filepath.Join(g.dir(), fleet.LockFile) }

func (g *globals) bindings() string {
	if g.bindingsPath != "" {
		return g.bindingsPath
	}
	return filepath.Join(g.dir(), cli.BindingsFile)
}

func (g *globals) presetDir(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(g.dir(), p)
}

// readFleet skips validation: commands other than up only need the name and
// per-Computer details.
func (g *globals) readFleet() (*fleet.Fleet, error) {
	data, err := os.ReadFile(g.fleetPath)
	if err != nil {
		return nil, fmt.Errorf("read fleet: %w  fix: run in the Fleet's directory, or point at it with --fleet", err)
	}
	f, err := fleet.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", g.fleetPath, err)
	}
	if f.Metadata.Name == "" {
		return nil, fmt.Errorf("%s: metadata.name is required", g.fleetPath)
	}
	return f, nil
}

func (g *globals) connect() (*cli.Client, *fleet.Fleet, error) {
	f, err := g.readFleet()
	if err != nil {
		return nil, nil, err
	}
	c, err := cli.Dial(f.Metadata.Name)
	if err != nil {
		return nil, nil, err
	}
	return c, f, nil
}

func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func computerPath(name, suffix string) string {
	return "/v1/agent-computers/" + url.PathEscape(name) + suffix
}

func status(ctx context.Context, c *cli.Client) (*api.StatusResponse, error) {
	var st api.StatusResponse
	if err := c.Do(ctx, "GET", "/v1/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
