package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

func runPrompt(g *globals, args []string) error {
	fs := g.flags("prompt")
	fresh := fs.Bool("new", false, "start a fresh session instead of continuing")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usageError("prompt needs a Computer name and the prompt text")
	}
	name, text := pos[0], strings.Join(pos[1:], " ")
	c, f, err := g.connect()
	if err != nil {
		return err
	}
	kind := f.Computers[name].Harness.Kind

	// Ctrl-C: before the turn is accepted, abort; after, ask the Computer to
	// stop and keep streaming until turn.finished; a second Ctrl-C quits.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var turnID string
	interrupted := false
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for range sigs {
			mu.Lock()
			id, first := turnID, !interrupted
			interrupted = true
			mu.Unlock()
			if id == "" || !first {
				cancel()
				return
			}
			dim(os.Stderr, "stopping turn "+id+" (Ctrl-C again to stop waiting)")
			go func() {
				sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
				defer scancel()
				if err := c.Do(sctx, "POST", computerPath(name, "/turns/"+url.PathEscape(id)+"/stop"), struct{}{}, nil); err != nil {
					fmt.Fprintf(os.Stderr, "a12s: stop %s: %v\n", id, err)
				}
			}()
		}
	}()

	session := api.SessionCurrent
	if *fresh {
		session = api.SessionNew
	}
	req := api.TurnRequest{RequestID: cli.NewRequestID(), Session: session, Prompt: text}
	var tr api.TurnResponse
	// The requestId makes a retry after a lost response safe: same turn.
	for attempt := 1; ; attempt++ {
		err = c.Do(ctx, "POST", computerPath(name, "/turns"), req, &tr)
		var apiErr *api.Error
		if err == nil || errors.As(err, &apiErr) || ctx.Err() != nil || attempt == 3 {
			break
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	if err != nil {
		return promptError(name, err)
	}
	mu.Lock()
	turnID = tr.TurnID
	mu.Unlock()

	tp := &turnPrinter{turn: tr.TurnID, kind: kind, out: os.Stdout, errOut: os.Stderr, tty: isTerminal(os.Stdout)}
	s := &cli.Stream{
		Client: c,
		Path:   computerPath(name, "/events?turn="+url.QueryEscape(tr.TurnID)),
		OnReconnect: func(after int64, err error) {
			dim(os.Stderr, fmt.Sprintf("[stream] reconnecting after seq %d: %v", after, err))
		},
	}
	if err := s.Run(ctx, tp.handle); err != nil {
		tp.endLine()
		return err
	}
	mu.Lock()
	wasInterrupted := interrupted
	mu.Unlock()
	switch {
	case tp.status == api.TurnCompleted:
		return nil
	case wasInterrupted:
		return exitCode(130)
	}
	return exitCode(1)
}

func promptError(name string, err error) error {
	switch {
	case cli.HasCode(err, api.ErrBusy):
		return fmt.Errorf("%s is busy with another turn (a12s stop %s interrupts it): %w", name, name, err)
	case cli.HasCode(err, api.ErrAsleep):
		return fmt.Errorf("%s is asleep (a12s wake %s): %w", name, name, err)
	}
	return err
}

// turnPrinter renders one turn's events: text to stdout as it arrives, the
// start and end lines around it, everything else as one dim stderr line.
type turnPrinter struct {
	turn, kind  string
	out, errOut io.Writer
	tty         bool // stdout is a terminal (shared with stderr)
	midLine     bool // stdout's last text did not end a line
	status      string
}

func (p *turnPrinter) handle(e api.Event) (bool, error) {
	if e.Turn != "" && e.Turn != p.turn {
		return false, nil
	}
	switch e.Type {
	case api.EventTurnStarted:
		var d api.TurnStartedData
		_ = json.Unmarshal(e.Data, &d)
		p.endLine()
		fmt.Fprintln(p.out, cli.TurnStartedLine(p.turn, p.kind, d))
	case api.EventTextDelta:
		var d api.TextDeltaData
		_ = json.Unmarshal(e.Data, &d)
		if d.Text != "" {
			io.WriteString(p.out, d.Text)
			p.midLine = !strings.HasSuffix(d.Text, "\n")
		}
	case api.EventTurnFinished:
		var d api.TurnFinishedData
		_ = json.Unmarshal(e.Data, &d)
		p.status = d.Status
		p.endLine()
		fmt.Fprintln(p.out, cli.TurnFinishedLine(p.turn, d))
		return true, nil
	default:
		if p.tty {
			p.endLine() // keep the dim line off the end of a text line
		}
		dim(p.errOut, cli.EventLine(e))
	}
	return false, nil
}

func (p *turnPrinter) endLine() {
	if p.midLine {
		fmt.Fprintln(p.out)
		p.midLine = false
	}
}

// dim writes one line, faint when w is a terminal.
func dim(w io.Writer, line string) {
	if f, ok := w.(*os.File); ok && isTerminal(f) {
		fmt.Fprintf(w, "\x1b[2m%s\x1b[0m\n", line)
		return
	}
	fmt.Fprintln(w, line)
}

func runStop(g *globals, args []string) error {
	fs := g.flags("stop")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("stop needs one Computer name")
	}
	name := pos[0]
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
	for _, cs := range st.Computers {
		if cs.Name != name {
			continue
		}
		if cs.ActiveTurn == "" {
			fmt.Printf("%s has no active turn\n", name)
			return nil
		}
		if err := c.Do(ctx, "POST", computerPath(name, "/turns/"+url.PathEscape(cs.ActiveTurn)+"/stop"), struct{}{}, nil); err != nil {
			return err
		}
		fmt.Printf("stopping turn %s on %s\n", cs.ActiveTurn, name)
		return nil
	}
	return fmt.Errorf("fleet %s has no Computer %q", st.Fleet, name)
}

func runLogs(g *globals, args []string) error {
	fs := g.flags("logs")
	follow := fs.Bool("follow", false, "keep streaming new events")
	since := fs.Int64("since", 0, "start after this seq")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("logs needs one Computer name")
	}
	if *since < 0 {
		return usageError("--since must not be negative")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	s := &cli.Stream{
		Client:     c,
		Path:       computerPath(pos[0], "/events"),
		After:      *since,
		SendCursor: true,
	}
	if !*follow {
		// The stream never ends by itself; a replay is done once it goes quiet.
		s.Idle = 2 * time.Second
	}
	err = s.Run(ctx, func(e api.Event) (bool, error) {
		fmt.Println(cli.LogLine(e))
		return false, nil
	})
	if errors.Is(err, context.Canceled) && *follow {
		return nil
	}
	return err
}
