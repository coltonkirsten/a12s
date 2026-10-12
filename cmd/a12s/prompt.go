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
	"slices"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An interactive command shouldn't hang until somebody wakes the Computer.
	st, err := status(ctx, c)
	if err != nil {
		return err
	}
	var cs *api.ComputerStatus
	for i := range st.Computers {
		if st.Computers[i].Name == name {
			cs = &st.Computers[i]
		}
	}
	if cs == nil {
		return fmt.Errorf("fleet %s has no Computer %q", st.Fleet, name)
	}
	if cs.Phase == api.PhaseAsleep || cs.Phase == api.PhaseStopping {
		if p := cs.Power; cs.Phase == api.PhaseAsleep && p != nil && p.AutoWake == api.AutoWakeOn && slices.Contains(p.WakeOnMessage, api.PrincipalLocal) {
			dim(os.Stderr, "waking "+name+" (power policy)")
		} else {
			msg := name + " is asleep"
			if p != nil && p.AutoWake != api.AutoWakeOn && p.AutoWake != api.AutoWakeOff {
				msg += " (auto-wake " + p.AutoWake + ": " + strings.ReplaceAll(p.Reason, "_", " ") + ")"
			}
			return &api.Error{Code: api.ErrAsleep, Message: msg, Fix: fmt.Sprintf("a12s wake %s, or a12s send %s \"…\" to leave a message", name, name)}
		}
	}

	// The idempotency key makes a retry after a lost response safe.
	req := api.EnqueueRequest{Body: text, Fresh: *fresh, IdempotencyKey: cli.NewRequestID()}
	var res api.EnqueueResponse
	for attempt := 1; ; attempt++ {
		err = c.Do(ctx, "POST", computerPath(name, "/inbox"), req, &res)
		var apiErr *api.Error
		if err == nil || errors.As(err, &apiErr) || ctx.Err() != nil || attempt == 3 {
			break
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if res.State == api.MsgPending && (res.Position > 0 || cs.ActiveTurn != "") {
		busy := ""
		if cs.ActiveTurn != "" {
			busy = " is busy with turn " + cs.ActiveTurn
		}
		dim(os.Stderr, fmt.Sprintf("queued: %s%s (%d messages ahead)", name, busy, res.Position))
	}

	// Ctrl-C latches a cancel intent: withdraw while pending (again on every
	// return), stop the Turn once delivered; a second Ctrl-C stops waiting.
	f2 := &promptFollow{c: c, name: name, id: res.ID}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for range sigs {
			if !f2.interrupt() {
				dim(os.Stderr, "stopped waiting; withdraw it later with a12s inbox rm "+name+" "+res.ID)
				cancel()
				return
			}
		}
	}()

	turn, err := f2.waitDelivered(ctx)
	if err != nil {
		return err
	}
	tp := &turnPrinter{turn: turn, kind: kind, mine: res.ID, out: os.Stdout, errOut: os.Stderr, tty: isTerminal(os.Stdout), json: g.json}
	if g.json {
		b, _ := json.Marshal(map[string]any{"message": res, "turn": turn})
		fmt.Println(string(b))
	}
	s := &cli.Stream{
		Client: c,
		Path:   computerPath(name, "/events?turn="+url.QueryEscape(turn)),
		OnReconnect: func(after int64, err error) {
			dim(os.Stderr, fmt.Sprintf("[stream] reconnecting after seq %d: %v", after, err))
		},
	}
	if err := s.Run(ctx, tp.handle); err != nil {
		tp.endLine()
		return err
	}
	switch {
	case tp.status == api.TurnCompleted:
		return nil
	case f2.wasInterrupted():
		return exitCode(130)
	}
	return exitCode(1)
}

type promptFollow struct {
	c    *cli.Client
	name string
	id   string

	mu          sync.Mutex
	interrupted bool
	turn        string
	presses     int
}

func (p *promptFollow) interrupt() bool {
	p.mu.Lock()
	p.presses++
	p.interrupted = true
	turn, presses := p.turn, p.presses
	p.mu.Unlock()
	if presses > 1 {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if turn != "" {
			dim(os.Stderr, "stopping turn "+turn+" (Ctrl-C again to stop waiting)")
			if err := p.c.Do(ctx, "POST", computerPath(p.name, "/turns/"+url.PathEscape(turn)+"/stop"), struct{}{}, nil); err != nil {
				fmt.Fprintf(os.Stderr, "a12s: stop %s: %v\n", turn, err)
			}
			return
		}
		p.withdraw(ctx)
	}()
	return true
}

func (p *promptFollow) withdraw(ctx context.Context) {
	err := p.c.Do(ctx, "DELETE", computerPath(p.name, "/inbox/"+url.PathEscape(p.id)), nil, nil)
	switch {
	case err == nil:
		dim(os.Stderr, "withdrew "+p.id)
	case cli.HasCode(err, api.ErrAlreadyClaimed):
		dim(os.Stderr, "being delivered; will stop its turn")
	default:
		fmt.Fprintf(os.Stderr, "a12s: withdraw %s: %v\n", p.id, err)
	}
}

func (p *promptFollow) wasInterrupted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interrupted
}

func (p *promptFollow) waitDelivered(ctx context.Context) (string, error) {
	var turn string
	var end error
	s := &cli.Stream{Client: p.c, Path: computerPath(p.name, "/inbox/"+url.PathEscape(p.id)+"/events")}
	state := func(st, turnID string, data map[string]any) bool {
		switch st {
		case api.MsgDelivered:
			turn = turnID
			p.mu.Lock()
			p.turn = turn
			intent := p.interrupted
			p.mu.Unlock()
			if intent { // the cancel intent survives the claim: stop that Turn
				go func() {
					sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_ = p.c.Do(sctx, "POST", computerPath(p.name, "/turns/"+url.PathEscape(turn)+"/stop"), struct{}{}, nil)
				}()
			}
			return true
		case "returned":
			dim(os.Stderr, "redelivering")
			if p.wasInterrupted() {
				go func() {
					sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					p.withdraw(sctx)
				}()
			}
		case api.MsgWithdrawn:
			if p.wasInterrupted() {
				end = exitCode(130)
			} else {
				end = fmt.Errorf("message %s was withdrawn", p.id)
			}
			return true
		case api.MsgDiscarded, api.MsgExpired:
			end = fmt.Errorf("message %s was %s before delivery", p.id, st)
			return true
		case api.MsgReplaced:
			end = fmt.Errorf("message %s was replaced by %v", p.id, data["by"])
			return true
		}
		return false
	}
	held := func(reason, diag string) {
		fix := "a12s inbox resume " + p.name
		if reason == api.HoldResumeFailed {
			fix += " (after a repair), or a12s inbox resume " + p.name + " --new"
		}
		msg := p.name + "'s inbox is held (" + reason + ")"
		if diag != "" {
			msg += ": " + diag
		}
		end = &api.Error{Code: api.ErrorCode("held"), Message: msg + "; " + p.id + " waits in the inbox", Fix: fix}
	}
	err := s.Run(ctx, func(e api.Event) (bool, error) {
		var data map[string]any
		_ = json.Unmarshal(e.Data, &data)
		switch string(e.Type) {
		case "snapshot":
			var snap struct {
				Message    api.Message `json:"message"`
				Held       bool        `json:"held"`
				HoldReason string      `json:"holdReason"`
				Diagnostic string      `json:"diagnostic"`
			}
			_ = json.Unmarshal(e.Data, &snap)
			if state(snap.Message.State, snap.Message.Turn, nil) {
				return true, nil
			}
			if snap.Held {
				held(snap.HoldReason, snap.Diagnostic)
				return true, nil
			}
		case "held":
			r, _ := data["reason"].(string)
			d, _ := data["diagnostic"].(string)
			held(r, d)
			return true, nil
		default:
			t, _ := data["turn"].(string)
			if state(string(e.Type), t, data) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return "", err
	}
	return turn, end
}

type turnPrinter struct {
	turn, kind  string
	mine        string // the caller's Message id, for the batch line
	out, errOut io.Writer
	tty         bool // stdout is a terminal (shared with stderr)
	json        bool // print each event as a JSON line instead
	midLine     bool // stdout's last text did not end a line
	status      string
}

func (p *turnPrinter) handle(e api.Event) (bool, error) {
	if e.Turn != "" && e.Turn != p.turn {
		return false, nil
	}
	if p.json {
		b, err := json.Marshal(e)
		if err != nil {
			return true, err
		}
		fmt.Fprintln(p.out, string(b))
		if e.Type == api.EventTurnFinished {
			var d api.TurnFinishedData
			_ = json.Unmarshal(e.Data, &d)
			p.status = d.Status
			return true, nil
		}
		return false, nil
	}
	switch e.Type {
	case api.EventTurnStarted:
		var d api.TurnStartedData
		_ = json.Unmarshal(e.Data, &d)
		p.endLine()
		fmt.Fprintln(p.out, cli.BatchStartedLine(p.turn, p.kind, p.mine, d))
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

func dim(w io.Writer, line string) {
	if f, ok := w.(*os.File); ok && isTerminal(f) {
		fmt.Fprintf(w, "\x1b[2m%s\x1b[0m\n", line)
		return
	}
	fmt.Fprintln(w, line)
}

func runStop(g *globals, args []string) error {
	fs := g.flags("stop")
	hold := fs.Bool("hold", false, "hold the inbox first, so nothing is delivered after the stop")
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
			if *hold {
				if err := c.Do(ctx, "POST", computerPath(name, "/inbox/hold"), api.HoldRequest{Reason: "a12s stop --hold"}, nil); err != nil {
					return err
				}
				if g.json {
					return printJSON(map[string]any{"computer": name, "turn": nil, "held": true})
				}
				fmt.Printf("%s has no active turn; its inbox is held (a12s inbox resume %s)\n", name, name)
				return nil
			}
			if g.json {
				return printJSON(map[string]any{"computer": name, "turn": nil, "held": false})
			}
			fmt.Printf("%s has no active turn\n", name)
			return nil
		}
		q := ""
		if *hold {
			q = "?hold=true"
		}
		if err := c.Do(ctx, "POST", computerPath(name, "/turns/"+url.PathEscape(cs.ActiveTurn)+"/stop"+q), struct{}{}, nil); err != nil {
			return err
		}
		if g.json {
			return printJSON(map[string]any{"computer": name, "turn": cs.ActiveTurn, "status": "stopping", "held": *hold})
		}
		fmt.Printf("stopping turn %s on %s\n", cs.ActiveTurn, name)
		if *hold {
			fmt.Printf("%s's inbox is held (a12s inbox resume %s)\n", name, name)
		}
		return nil
	}
	return fmt.Errorf("fleet %s has no Computer %q", st.Fleet, name)
}

func runLogs(g *globals, args []string) error {
	follow := new(bool)
	fs := g.flagsFollow("logs", follow)
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
		UntilHead:  !*follow,
	}
	err = s.Run(ctx, func(e api.Event) (bool, error) {
		if g.json {
			b, err := json.Marshal(e)
			if err != nil {
				return true, err
			}
			fmt.Println(string(b))
			return false, nil
		}
		fmt.Println(cli.LogLine(e))
		return false, nil
	})
	if errors.Is(err, context.Canceled) && *follow {
		return nil
	}
	return err
}
