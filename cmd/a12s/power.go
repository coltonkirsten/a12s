package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

func runSleep(g *globals, args []string) error { return power(g, "sleep", api.PowerAsleep, args) }

// runWake re-reads the bindings and sends each Computer its credentials:
// injected credentials do not survive sleep.
func runWake(g *globals, args []string) error { return power(g, "wake", api.PowerAwake, args) }

func power(g *globals, verb string, state api.Power, args []string) error {
	fs := g.flags(verb)
	all := fs.Bool("all", false, "every Computer in the Fleet")
	var clearInbox *bool
	if state == api.PowerAwake {
		clearInbox = fs.Bool("clear-inbox", false, "discard the pending messages (journaled, not erased) before the Computer takes work")
	}
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *all == (len(names) > 0) {
		return usageError(verb + " needs Computer names or --all")
	}
	c, f, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	if *all {
		st, err := status(ctx, c)
		if err != nil {
			return err
		}
		for _, cs := range st.Computers {
			names = append(names, cs.Name)
		}
	}

	reqs := make([]api.PowerRequest, len(names))
	if state == api.PowerAwake {
		b, err := cli.LoadBindings(g.bindings())
		if err != nil {
			return err
		}
		// Resolve everything before waking anything.
		for i, n := range names {
			creds, err := b.Resolve(cli.Needed(f, n))
			if err != nil {
				return err
			}
			reqs[i].Credentials = creds
		}
	}

	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		reqs[i].State = state
		if clearInbox != nil && *clearInbox {
			// One requestId per Computer: a retry is the same clear.
			reqs[i].ClearInbox, reqs[i].RequestID = true, cli.NewRequestID()
		}
		wg.Go(func() {
			var cs api.ComputerStatus
			if err := c.Do(ctx, "POST", computerPath(n, "/power"), reqs[i], &cs); err != nil {
				errs[i] = fmt.Errorf("%s %s: %w", verb, n, err)
				return
			}
			if cs.InboxCleared != nil {
				fmt.Printf("%s: discarded %d pending message(s) (journaled with their bodies)\n", n, *cs.InboxCleared)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func runApply(g *globals, args []string) error {
	fs := g.flags("apply")
	interrupt := fs.Bool("interrupt", false, "interrupt busy Computers' active turns")
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
