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
	"syscall"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// command is one entry of the subcommand table.
type command struct {
	name    string
	args    string
	summary string
	run     func(g *globals, args []string) error
}

func commands() []command {
	return []command{
		{"up", "[--asleep] [--controller-image ref]", "apply the Fleet file: create, update or stage Computers", runUp},
		{"apply", "--interrupt [--grace 30s]", "force a staged change onto a busy Computer", runApply},
		{"status", "[<name>]", "state, session, active turn, harness version, warnings", runStatus},
		{"prompt", "<name> [--new] \"…\"", "send a turn and stream its events", runPrompt},
		{"send", "<name> \"…\" [--fresh] [--expires d] [--replace k] [--meta k=v]", "enqueue a message and exit (works while asleep)", runSend},
		{"inbox", "<name> [--all] [--bodies] | rm|clear|hold|resume <name>", "list or manage a Computer's inbox", runInbox},
		{"token", "create <name> --actions a,b --computers c | ls | rm <name>", "scoped API tokens", runToken},
		{"stop", "<name> [--hold]", "interrupt the active turn (--hold: hold the inbox first)", runStop},
		{"logs", "<name> [--follow] [--since <seq>]", "replay and follow the journal", runLogs},
		{"sleep", "<name>... | --all", "sleep Computers (storage kept, no CPU or RAM)", runSleep},
		{"wake", "<name>... | --all [--clear-inbox]", "wake Computers; resume happens on the next prompt", runWake},
		{"shell", "<name> [--root] [-- command...]", "audited terminal inside a Computer, outside the harness", runShell},
		{"drive", "ls | rm <name> [--yes]", "list drives; rm and down --purge are the only commands that destroy data", runDrive},
		{"down", "[--purge [--yes]]", "remove Computers, Controller and network; keep drives (--purge destroys them)", runDown},
		{"controller", "", "run the Controller (inside its container)", runControllerCmd},
	}
}

// globals are the flags every command accepts, before or after its name.
type globals struct {
	fleetPath    string
	bindingsPath string
}

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	g := &globals{fleetPath: "fleet.yaml"}
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
	fmt.Fprintln(w, "usage: a12s [-f fleet.yaml] [--bindings path] <command> [args]")
	fmt.Fprintln(w)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %-38s %s\n", c.name, c.args, c.summary)
	}
}

// usageError is a bad invocation: exit code 2.
type usageError string

func (e usageError) Error() string { return string(e) }

// exitCode ends the program with a code and no message (the command has
// already said what happened).
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
	fs.StringVar(&g.fleetPath, "f", g.fleetPath, "Fleet file; fleet.lock and "+cli.BindingsFile+" live beside it")
	fs.StringVar(&g.bindingsPath, "bindings", g.bindingsPath, "bindings file (default: "+cli.BindingsFile+" beside the Fleet file)")
}

// flags returns a FlagSet for a command that also accepts the global flags.
func (g *globals) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	return fs
}

// parse parses flags anywhere among the arguments (`a12s prompt scout --new
// "…"`) and returns the positional ones. Everything after "--" is positional.
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

// presetDir resolves a preset path as written in the Fleet (relative to the Fleet file).
func (g *globals) presetDir(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(g.dir(), p)
}

// readFleet parses the Fleet file without validating it: commands other
// than up only need its name and per-Computer details.
func (g *globals) readFleet() (*fleet.Fleet, error) {
	data, err := os.ReadFile(g.fleetPath)
	if err != nil {
		return nil, fmt.Errorf("read fleet: %w (use -f to point at the Fleet file)", err)
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

// connect reads the Fleet file and finds its Controller.
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

// interruptible returns a context cancelled by Ctrl-C or SIGTERM.
func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// computerPath is /v1/computers/<name><suffix>.
func computerPath(name, suffix string) string {
	return "/v1/computers/" + url.PathEscape(name) + suffix
}

// status fetches GET /v1/status.
func status(ctx context.Context, c *cli.Client) (*api.StatusResponse, error) {
	var st api.StatusResponse
	if err := c.Do(ctx, "GET", "/v1/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// isTerminal reports whether f is a character device (a terminal).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
