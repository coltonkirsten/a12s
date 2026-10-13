package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

type upOptions struct {
	image string
}

func runUp(g *globals, args []string) error {
	fs := g.flags("up")
	var o upOptions
	fs.StringVar(&o.image, "controller-image", cli.DefaultControllerImage, "Controller image")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("up takes no arguments")
	}
	return g.up(o)
}

// loadSpec reads the Fleet file as the API takes it: seccomp profiles inline.
func (g *globals) loadSpec() (*fleet.Spec, *fleet.Fleet, error) {
	spec, err := fleet.LoadSpec(g.fleetPath)
	if err != nil {
		return nil, nil, err
	}
	if err := g.inline(spec); err != nil {
		return nil, nil, err
	}
	f, err := spec.Resolve()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", g.fleetPath, err)
	}
	return spec, f, nil
}

// up is a plain apply: the live Fleet becomes the Fleet file, whatever changed through the API since.
func (g *globals) up(o upOptions) error {
	spec, f, err := g.loadSpec()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	cli.PullMissing(ctx, cli.FleetImages(f), os.Stderr)
	c, err := cli.EnsureController(ctx, cli.StartOptions{
		Fleet: f.Metadata.Name,
		Image: o.image,
		Log:   func(s string) { fmt.Fprintln(os.Stderr, s) },
	})
	if err != nil {
		return err
	}
	var resp api.PutFleetResponse
	if err := c.Do(ctx, "PUT", "/v1/fleet", api.PutFleetRequest{Fleet: spec}, &resp); err != nil {
		return err
	}
	if err := g.finishUp(&resp, os.Stdout, os.Stderr); err != nil {
		return err
	}
	if !g.json {
		for _, line := range cli.SeccompLines(f) {
			fmt.Fprintln(os.Stdout, line)
		}
	}
	return nil
}

// finishUp reports what converged; any failure still fails the command.
func (g *globals) finishUp(resp *api.PutFleetResponse, stdout, stderr io.Writer) error {
	failed := cli.UpErrors(resp)
	if g.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return err
		}
		if len(failed) > 0 {
			return exitCode(1)
		}
		return nil
	}
	for _, line := range cli.UpSummary(resp) {
		fmt.Fprintln(stdout, line)
	}
	for _, line := range failed {
		fmt.Fprintln(stderr, line)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d agent computer(s) or Service(s) did not converge (a12s status shows their state)", len(failed))
	}
	return nil
}

// inline turns what only a Fleet file can say into what the API takes: each security.seccomp path becomes the
// profile itself.
func (g *globals) inline(spec *fleet.Spec) error {
	one := func(field string, raw json.RawMessage) (json.RawMessage, error) {
		if len(raw) == 0 {
			return raw, nil
		}
		var c fleet.Computer
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		p := c.Security.Seccomp.Path
		if p == "" {
			return raw, nil
		}
		data, err := os.ReadFile(g.fleetRel(p))
		if err != nil {
			return nil, fmt.Errorf("%s.security.seccomp: %v", field, err)
		}
		if _, err := fleet.ValidateSeccomp(data); err != nil {
			return nil, fmt.Errorf("%s.security.seccomp: %s: %v", field, p, err)
		}
		if err := c.Security.Seccomp.UnmarshalJSON(data); err != nil {
			return nil, fmt.Errorf("%s.security.seccomp: %s: %v", field, p, err)
		}
		return fleet.SpecOf(c), nil
	}
	var err error
	if spec.Defaults, err = one("defaults", spec.Defaults); err != nil {
		return err
	}
	for _, name := range spec.Names() {
		if spec.AgentComputers[name], err = one("agent_computers."+name, spec.AgentComputers[name]); err != nil {
			return err
		}
	}
	return nil
}
