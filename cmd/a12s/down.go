package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// runDown is `a12s down [--purge [--yes]]`. It removes the Fleet's
// Computers, Controller, networks and Controller state, and keeps its drives.
// --purge also destroys the drives, after a confirmation that lists them:
// with `drive rm`, the only commands that destroy data.
func runDown(g *globals, args []string) error {
	fs := g.flags("down")
	purge := fs.Bool("purge", false, "also destroy the Fleet's drives (asks first)")
	yes := fs.Bool("yes", false, "with --purge: do not ask for confirmation")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("down takes no arguments")
	}
	if *yes && !*purge {
		return usageError("--yes only applies to --purge")
	}
	f, err := g.readFleet()
	if err != nil {
		return err
	}
	name := f.Metadata.Name
	if !fleet.ValidName(name) {
		return fmt.Errorf("%s: metadata.name %q must be a lowercase DNS label of at most 40 characters", g.fleetPath, name)
	}

	ctx, cancel := interruptible()
	defer cancel()
	dc, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer dc.Close()
	plan, err := cli.PlanDown(ctx, dc, name)
	if err != nil {
		return err
	}
	if *purge && len(plan.Drives) > 0 && !*yes {
		if !confirmPurge(os.Stdin, os.Stderr, name, plan.Drives) {
			return exitCode(1)
		}
	}
	res, derr := cli.Down(ctx, dc, plan, *purge)
	writeDown(os.Stdout, name, plan, res)
	return derr
}

// confirmPurge lists the drives and asks y/N, like `a12s drive rm`.
func confirmPurge(in io.Reader, out io.Writer, fleetName string, drives []cli.Drive) bool {
	fmt.Fprintf(out, "a12s down --purge destroys the %d drive(s) of fleet %s and everything on them:\n", len(drives), fleetName)
	for _, d := range drives {
		fmt.Fprintf(out, "  %s (volume %s)\n", d.Name, d.Volume)
	}
	fmt.Fprint(out, "This cannot be undone. [y/N] ")
	if !confirmed(in) {
		fmt.Fprintln(out, "nothing removed")
		return false
	}
	return true
}

// confirmed reads one line and reports whether it is y or yes.
func confirmed(in io.Reader) bool {
	line, _ := bufio.NewReader(in).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func writeDown(w io.Writer, fleetName string, plan *cli.DownPlan, res *cli.DownResult) {
	if plan.Empty() && len(res.Destroyed) == 0 {
		fmt.Fprintf(w, "fleet %s is already down\n", fleetName)
	}
	for _, c := range res.Containers {
		fmt.Fprintln(w, "removed container "+c)
	}
	for _, n := range res.Networks {
		fmt.Fprintln(w, "removed network "+n)
	}
	for _, v := range res.Volumes {
		fmt.Fprintln(w, "removed volume "+v)
	}
	for _, f := range res.StateFiles {
		fmt.Fprintln(w, "removed "+f)
	}
	if res.StateDir != "" {
		fmt.Fprintln(w, "removed "+res.StateDir)
	}
	for _, d := range res.Destroyed {
		fmt.Fprintf(w, "destroyed drive %s (volume %s)\n", d.Name, d.Volume)
	}
	if len(res.Kept) > 0 {
		fmt.Fprintf(w, "kept %d drive(s); a12s up reattaches them:\n", len(res.Kept))
		for _, d := range res.Kept {
			fmt.Fprintf(w, "  %s (volume %s)\n", d.Name, d.Volume)
		}
		fmt.Fprintln(w, "to destroy them: a12s down --purge (or a12s drive rm <name> while the fleet is up)")
	}
}
