package main

import (
	"fmt"
	"os"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

func runStatus(g *globals, args []string) error {
	fs := g.flags("status")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageError("status takes at most one Computer or Service name")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	st, err := status(ctx, c)
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		var only []api.ComputerStatus
		for _, cs := range st.Computers {
			if cs.Name == pos[0] {
				only = append(only, cs)
			}
		}
		var svc []api.ServiceStatus
		for _, ss := range st.Services {
			if ss.Name == pos[0] {
				svc = append(svc, ss)
			}
		}
		if len(only) == 0 && len(svc) == 0 {
			return fmt.Errorf("fleet %s has no Computer or Service %q", st.Fleet, pos[0])
		}
		st.Computers, st.Services = only, svc
	} else if len(st.Computers) == 0 {
		fmt.Println("no Computers (run a12s up)")
	}
	cli.WriteStatus(os.Stdout, st)
	return nil
}
