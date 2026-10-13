package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func runSleep(g *globals, args []string) error { return power(g, "sleep", api.PowerAsleep, args) }

// runWake re-sends credentials because injected credentials do not survive sleep.
func runWake(g *globals, args []string) error { return power(g, "wake", api.PowerAwake, args) }

func power(g *globals, verb string, state api.Power, args []string) error {
	fs := g.flags(verb)
	all := fs.Bool("all", false, "every agent computer in the Fleet")
	var hold *bool
	if state == api.PowerAsleep {
		hold = fs.Bool("hold", false, "keep it asleep until a12s wake: no message wakes it, whatever its wakeOnMessage")
	}
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *all == (len(names) > 0) {
		return usageError(verb + " needs agent computer or Service names, or --all")
	}
	c, f, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var services []string
	if *all {
		st, err := status(ctx, c)
		if err != nil {
			return err
		}
		for _, cs := range st.AgentComputers {
			names = append(names, cs.Name)
		}
		for _, ss := range st.Services {
			services = append(services, ss.Name)
		}
	} else {
		if names, services, err = splitTargets(names, f.HasService, func(n string) bool { _, ok := f.Computers[n]; return ok }); err != nil {
			return err
		}
	}
	if len(services) > 0 {
		if !*all && hold != nil && *hold {
			return usageError("--hold is for agent computers; a Service has no power policy")
		}
		if err := powerServices(ctx, g, c, f, verb, state, services); err != nil {
			return err
		}
	}
	if len(names) == 0 {
		return nil
	}

	reqs := make([]api.PowerRequest, len(names))

	errs := make([]error, len(names))
	results := make([]api.ComputerStatus, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		reqs[i].State = state
		if hold != nil && *hold {
			reqs[i].Hold = true
		}
		wg.Go(func() {
			var cs api.ComputerStatus
			if err := c.Do(ctx, "POST", computerPath(n, "/power"), reqs[i], &cs); err != nil {
				errs[i] = fmt.Errorf("%s %s: %w", verb, n, err)
				return
			}
			results[i] = cs
		})
	}
	wg.Wait()
	if g.json {
		if err := printJSON(map[string]any{"computers": results}); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

func runApply(g *globals, args []string) error {
	fs := g.flags("apply")
	interrupt := fs.Bool("interrupt", false, "interrupt busy agent computers' active turns")
	grace := fs.Duration("grace", 30*time.Second, "how long an interrupted turn may take to finish")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("apply takes no arguments")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var resp api.ApplyResponse
	req := api.ApplyRequest{Interrupt: *interrupt, Grace: api.Duration(*grace)}
	if err := c.Do(ctx, "POST", "/v1/apply", req, &resp); err != nil {
		return err
	}
	if g.json {
		return printJSON(resp)
	}
	if len(resp.Applied) == 0 && len(resp.Pending) == 0 {
		fmt.Println("nothing staged")
	}
	if len(resp.Applied) > 0 {
		fmt.Println("applied " + strings.Join(resp.Applied, ", "))
	}
	if len(resp.Pending) > 0 {
		hint := ""
		if !*interrupt {
			hint = " (busy; --interrupt to force)"
		}
		fmt.Println("pending " + strings.Join(resp.Pending, ", ") + hint)
	}
	return nil
}

func powerServices(ctx context.Context, g *globals, c *cli.Client, f *fleet.Fleet, verb string, state api.Power, names []string) error {
	reqs := make([]api.ServicePowerRequest, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		reqs[i].State = state
		wg.Go(func() {
			var ss api.ServiceStatus
			if err := c.Do(ctx, "POST", servicePath(n, "/power"), reqs[i], &ss); err != nil {
				errs[i] = fmt.Errorf("%s %s: %w", verb, n, err)
				return
			}
			if ss.Error != nil {
				errs[i] = fmt.Errorf("%s %s: %s", verb, n, ss.Error.Reason)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
