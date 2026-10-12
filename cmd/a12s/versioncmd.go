package main

import (
	"fmt"

	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/version"
)

func runVersion(g *globals, args []string) error {
	if _, err := parse(g.flags("version"), args); err != nil {
		return err
	}
	if g.json {
		return printJSON(map[string]string{"version": version.Version, "registry": version.Registry, "controller": cli.DefaultControllerImage})
	}
	fmt.Printf("a12s %s (images from %s)\n", version.Version, version.Registry)
	return nil
}
