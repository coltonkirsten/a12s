package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

func runDrive(g *globals, args []string) error {
	if len(args) == 0 {
		return usageError("drive needs a subcommand: ls or rm")
	}
	switch args[0] {
	case "ls":
		return driveLs(g, args[1:])
	case "rm":
		return driveRm(g, args[1:])
	}
	return usageError("unknown drive subcommand " + args[0])
}

func driveLs(g *globals, args []string) error {
	pos, err := parse(g.flags("drive ls"), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("drive ls takes no arguments")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var resp api.DrivesResponse
	if err := c.Do(ctx, "GET", "/v1/drives", nil, &resp); err != nil {
		return err
	}
	cli.WriteDrives(os.Stdout, resp.Drives)
	return nil
}

// driveRm is the only command that destroys data, so it asks first.
func driveRm(g *globals, args []string) error {
	fs := g.flags("drive rm")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("drive rm needs one drive name")
	}
	name := pos[0]
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "Destroy drive %s and everything on it? This cannot be undone. [y/N] ", name)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(os.Stderr, "kept "+name)
			return exitCode(1)
		}
	}
	ctx, cancel := interruptible()
	defer cancel()
	if err := c.Do(ctx, "DELETE", "/v1/drives/"+url.PathEscape(name), nil, nil); err != nil {
		if cli.HasCode(err, api.ErrAttached) {
			return fmt.Errorf("drive %s is attached; remove it from the Fleet (and run a12s up) first: %w", name, err)
		}
		return err
	}
	fmt.Println("removed drive " + name)
	return nil
}
