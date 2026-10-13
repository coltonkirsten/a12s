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
