package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/driver/gce"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// runDown keeps drives; --purge destroys them only after a confirmation that
// lists them. With `drive rm`, these are the only commands that destroy data.
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
	drv, err := cli.ReadDriverConfig(name)
	if err != nil {
		return err
	}
	var td *gce.Teardown
	var vms gce.TeardownPlan
	if drv.Driver == gce.Name {
		if td, err = gce.OpenTeardown(ctx, name, drv.GCE); err != nil {
			return err
		}
		if vms, err = td.Plan(ctx); err != nil {
			return err
		}
	}
	var purgeable []cli.Drive // a drive with a malformed (empty) label is always kept
	for _, d := range plan.Drives {
		if d.ID != "" {
			purgeable = append(purgeable, d)
		}
	}
	for _, d := range vms.Disks {
		purgeable = append(purgeable, cli.Drive{ID: d.ID, Name: d.ID, Disk: d.Name})
	}
	if *purge && (len(purgeable) > 0 || len(plan.Data) > 0) && !*yes {
		if !confirmPurge(os.Stdin, os.Stderr, name, purgeable, plan.Data, inboxCounts(name)) {
			return exitCode(1)
		}
	}
	var vmErr error
	var gceRes gceDown
	if td != nil {
		// The VMs go first: they work without the Controller, and must not outlive it unnoticed.
		gceRes.VMs, gceRes.DisksDestroyed, vmErr = td.Down(ctx, vms, *purge)
		if !*purge {
			for _, d := range vms.Disks {
				gceRes.DisksKept = append(gceRes.DisksKept, d.Name)
			}
		}
	}
	res, derr := cli.Down(ctx, dc, plan, *purge)
	derr = errors.Join(vmErr, derr)
	if g.json {
		out := any(res)
		if td != nil {
			out = struct {
				*cli.DownResult
				GCE gceDown `json:"gce"`
			}{res, gceRes}
		}
		if err := printJSON(out); err != nil {
			return err
		}
		return derr
	}
	writeGCEDown(os.Stdout, gceRes)
	writeDown(os.Stdout, name, plan, res)
	return derr
}

// gceDown is what down did on GCE.
type gceDown struct {
	VMs            []string `json:"vms"`            // agent computers whose VMs were deleted
	DisksKept      []string `json:"disksKept"`      // home disks left in place
	DisksDestroyed []string `json:"disksDestroyed"` // home disks deleted (purge)
}

func writeGCEDown(w io.Writer, r gceDown) {
	for _, c := range r.VMs {
		fmt.Fprintln(w, "removed the VM of "+c)
	}
	for _, d := range r.DisksDestroyed {
		fmt.Fprintln(w, "destroyed disk "+d)
	}
	if len(r.DisksKept) > 0 {
		fmt.Fprintf(w, "kept %d disk(s); a12s up reattaches them: %s\n", len(r.DisksKept), strings.Join(r.DisksKept, ", "))
	}
}

func inboxCounts(fleetName string) string {
	c, err := cli.Dial(fleetName)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := status(ctx, c)
	if err != nil {
		return ""
	}
	var pending, computers, held int64
	add := func(in *api.InboxSummary) {
		if in == nil {
			return
		}
		if n := in.Pending + in.Claimed; n > 0 {
			pending += n
			computers++
		}
		if in.Held {
			held++
		}
	}
	for _, cs := range st.AgentComputers {
		add(cs.Inbox)
	}
	for i := range st.OrphanedInboxes {
		add(&st.OrphanedInboxes[i])
	}
	return fmt.Sprintf("inbox: %d pending across %d agent computers, %d held", pending, computers, held)
}

func confirmPurge(in io.Reader, out io.Writer, fleetName string, drives []cli.Drive, data []string, counts string) bool {
	fmt.Fprintf(out, "a12s down --purge destroys the %d drive(s) of fleet %s and everything on them:\n", len(drives), fleetName)
	for _, d := range drives {
		if d.Disk != "" {
			fmt.Fprintf(out, "  %s (disk %s)\n", d.Name, d.Disk)
		} else {
			fmt.Fprintf(out, "  %s (volume %s)\n", d.Name, d.Volume)
		}
	}
	for _, v := range data {
		if counts == "" {
			counts = "inbox counts unavailable: the Controller is not running"
		}
		fmt.Fprintf(out, "and the Fleet's data, undelivered messages included (volume %s; %s)\n", v, counts)
	}
	fmt.Fprint(out, "This cannot be undone. [y/N] ")
	if !confirmed(in) {
		fmt.Fprintln(out, "nothing removed")
		return false
	}
	return true
}

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
	for _, v := range res.DataDestroyed {
		fmt.Fprintf(w, "destroyed the Fleet's data (volume %s): inboxes, holds, tokens\n", v)
	}
	for _, v := range res.DataKept {
		fmt.Fprintf(w, "kept the Fleet's data (volume %s): inboxes, holds and tokens survive; a12s up reattaches it\n", v)
	}
}
