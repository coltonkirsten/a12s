package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

var credNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// runCreds sets a credential on the Fleet's Controller, starting it if needed; the value is kept nowhere else.
func runCreds(g *globals, args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return usageError("creds set <name>  (reads the value from stdin)")
	}
	fs := g.flags("creds set")
	noCheck := fs.Bool("no-check", false, "store the value even if it does not look like this credential")
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
		// stty acts on its stdin, which must be the terminal.
		stty := func(arg string) error {
			cmd := exec.Command("stty", arg)
			cmd.Stdin = os.Stdin
			return cmd.Run()
		}
		if err := stty("-echo"); err != nil {
			return fmt.Errorf("cannot turn terminal echo off (%v); pipe the value instead: a12s creds set %s < file", err, name)
		}
		defer stty("echo")
		fmt.Fprintf(os.Stderr, "paste %s and press Enter (not echoed): ", name)
		line, rerr := bufio.NewReader(os.Stdin).ReadBytes('\n')
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
	if !*noCheck {
		if err := checkCredentialShape(name, value); err != nil {
			return err
		}
	}
	if err := setCredential(f.Metadata.Name, name, value); err != nil {
		return err
	}
	if g.json {
		return printJSON(map[string]any{"credential": name})
	}
	fmt.Printf("%s set on the Fleet's Controller; agent computers and Services that name it get it now\n", name)
	return nil
}

// setCredential puts the value on the Fleet's Controller, starting one if none runs, so the order of creds set and
// a12s up never matters.
func setCredential(fleetName, name string, value []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := cli.EnsureController(ctx, cli.StartOptions{Fleet: fleetName, Image: cli.DefaultControllerImage, Log: func(s string) { fmt.Fprintln(os.Stderr, s) }})
	if err != nil {
		return err
	}
	return c.Do(ctx, "PUT", "/v1/credentials/"+name, api.CredentialRequest{Value: string(value)}, nil)
}

// serviceLogin runs the login in a memory-only container so the credential
// never touches disk there or on the host; it goes straight to the Controller.
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
	c, _, err := g.connect()
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
	if err := c.Do(ctx, "PUT", "/v1/credentials/"+l.Produces, api.CredentialRequest{Value: string(bytes.TrimSpace(out.Bytes()))}, nil); err != nil {
		return err
	}
	if g.json {
		return printJSON(map[string]string{"service": name, "credential": l.Produces})
	}
	fmt.Printf("logged in: %s set on the Controller; nothing is left in the container, and %s starts with it.\n", l.Produces, name)
	return nil
}

// checkCredentialShape catches the common paste mistakes (a wrapped setup token stored in pieces, a stray quote,
// the wrong secret) before a Turn fails with a bare 401.
func checkCredentialShape(name string, value []byte) error {
	if name != "anthropic" {
		return nil
	}
	v := string(value)
	const fix = "fix: pbpaste | tr -d ' \\n' | a12s creds set anthropic   (or printf %s \"$TOKEN\" | a12s creds set anthropic; --no-check skips this check)"
	if strings.ContainsAny(v, " \t\r\n") {
		return fmt.Errorf("the anthropic value has spaces or line breaks inside it: a token wrapped over several terminal lines was pasted in pieces\n%s", fix)
	}
	if !strings.HasPrefix(v, "sk-ant-oat01-") && !strings.HasPrefix(v, "sk-ant-api03-") {
		shown := v
		if len(shown) > 10 {
			shown = shown[:10]
		}
		return fmt.Errorf("the anthropic value starts %q, not sk-ant-oat01- (a claude setup-token token) or sk-ant-api03- (an Anthropic API key)\n%s", shown, fix)
	}
	if len(v) < 90 || len(v) > 400 {
		return fmt.Errorf("the anthropic value is %d characters; a setup token or API key is about 108: it was probably cut off while pasting\n%s", len(v), fix)
	}
	return nil
}
