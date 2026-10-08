package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

var credNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// runCreds is `a12s creds set <name>`: paste a token once; it is written to
// ~/.a12s/<fleet>/<name> (0600, atomically) and bound in the bindings file.
func runCreds(g *globals, args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return usageError("creds set <name>  (reads the value from stdin)")
	}
	fs := g.flags("creds set")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 || !credNameRE.MatchString(pos[0]) {
		return usageError("creds set needs one credential name (lowercase letters, digits, - and _)")
	}
	name := pos[0]
	f, err := g.readFleet()
	if err != nil {
		return err
	}
	var value []byte
	if isTerminal(os.Stdin) {
		fmt.Fprintf(os.Stderr, "paste %s and press Enter (not echoed): ", name)
		_ = exec.Command("stty", "-echo").Run()
		line, rerr := bufio.NewReader(os.Stdin).ReadBytes('\n')
		_ = exec.Command("stty", "echo").Run()
		fmt.Fprintln(os.Stderr)
		if rerr != nil && rerr != io.EOF {
			return rerr
		}
		value = line
	} else if value, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20)); err != nil {
		return err
	}
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return fmt.Errorf("no value for %s on stdin", name)
	}
	path, err := cli.CredentialPath(f.Metadata.Name, name)
	if err != nil {
		return err
	}
	if err := cli.WriteSecret(path, value); err != nil {
		return err
	}
	if err := cli.BindFile(g.bindings(), name, path); err != nil {
		return err
	}
	fmt.Printf("%s saved to %s (0600) and bound in %s; a12s up sends it\n", name, path, g.bindings())
	return nil
}

// serviceLogin is `a12s service login <svc>`: the Controller starts a
// memory-only container of the Service's pinned image; the login command
// runs in it on this terminal; the credential it wrote comes back over the
// exec stream and lands in ~/.a12s/<fleet>/<credential> (0600, atomic),
// bound like `a12s creds set`. The container, and every tmpfs in it, is then
// removed (enhancements §5.5).
func serviceLogin(g *globals, args []string) error {
	pos, err := parse(g.flags("service login"), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("service login needs one Service name")
	}
	name := pos[0]
	docker, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("a12s service login runs the docker CLI: %w", err)
	}
	c, f, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var l api.LoginResponse
	if err := c.Do(ctx, "POST", servicePath(name, "/login"), nil, &l); err != nil {
		return err
	}
	defer func() { _ = c.Do(ctx, "DELETE", servicePath(name, "/login"), nil, nil) }()
	fmt.Fprintf(os.Stderr, "logging in to %s (memory-only container; follow the instructions below)\n", name)
	argv := []string{"exec", "-i"}
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		argv = append(argv, "-t")
	}
	argv = append(append(argv, "-u", l.User, l.Container), l.Command...)
	run := exec.CommandContext(ctx, docker, argv...)
	run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := run.Run(); err != nil {
		return fmt.Errorf("the login command failed: %w", err)
	}
	read := exec.CommandContext(ctx, docker, append([]string{"exec", "-u", l.User, l.Container}, l.Read...)...)
	var out, errb bytes.Buffer
	read.Stdout, read.Stderr = &out, &errb
	if err := read.Run(); err != nil {
		return fmt.Errorf("reading the credential: %v: %s", err, bytes.TrimSpace(errb.Bytes()))
	}
	path, err := cli.CredentialPath(f.Metadata.Name, l.Produces)
	if err != nil {
		return err
	}
	if err := cli.WriteSecret(path, out.Bytes()); err != nil {
		return err
	}
	if err := cli.BindFile(g.bindings(), l.Produces, path); err != nil {
		return err
	}
	fmt.Printf("logged in: %s saved to %s (0600) and bound; nothing is left in the container. Run a12s up to start %s with it.\n", l.Produces, path, name)
	return nil
}
