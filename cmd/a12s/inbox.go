package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

type metaFlag map[string]string

func (m metaFlag) String() string { return "" }
func (m metaFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--meta wants key=value, got %q", v)
	}
	m[k] = val
	return nil
}

func parseExpires(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("--expires %q is not a duration (0, 90s, 15m, 1h, 7d)", s)
		}
		return n * 86400, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d%time.Second != 0 {
		return 0, fmt.Errorf("--expires %q is not a duration in whole seconds (0, 90s, 15m, 1h, 7d)", s)
	}
	return int64(d / time.Second), nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runSend(g *globals, args []string) error {
	fs := g.flags("send")
	from := fs.String("from", "", "sender label (default: your principal, local)")
	fresh := fs.Bool("fresh", false, "start a new session with this message")
	key := fs.String("key", "", "idempotency key (default: a random one, so a retry never queues twice)")
	replace := fs.String("replace", "", "replace key: supersede your pending message with this key")
	expires := fs.String("expires", "", "0 (now or never), or a window: 90s, 15m, 1h, 7d")
	asJSON := &g.json
	meta := metaFlag{}
	fs.Var(meta, "meta", "metadata key=value (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usageError(`send needs a Computer name and the message ("-" reads stdin)`)
	}
	name := pos[0]
	body := strings.Join(pos[1:], " ")
	if body == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		body = string(b)
	}
	req := api.EnqueueRequest{Body: body, Sender: *from, Fresh: *fresh, IdempotencyKey: *key, ReplaceKey: *replace}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = cli.NewRequestID()
	}
	if len(meta) > 0 {
		req.Metadata = meta
	}
	if *expires != "" {
		n, err := parseExpires(*expires)
		if err != nil {
			return usageError(err.Error())
		}
		req.Expires = &n
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var res api.EnqueueResponse
	// The idempotency key makes a retry after a lost response safe.
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
	if *asJSON {
		return printJSON(res)
	}
	switch res.State {
	case api.MsgPending:
		ahead := ""
		if res.Position > 0 {
			ahead = fmt.Sprintf(", %d ahead", res.Position)
		}
		fmt.Printf("queued %s for %s (seq %d%s; %s is %s)\n", res.ID, name, res.Seq, ahead, name, res.Phase)
	case api.MsgClaimed:
		fmt.Printf("delivering %s to %s now (batch %s)\n", res.ID, name, res.Batch)
	default:
		fmt.Printf("%s is %s (seq %d)\n", res.ID, res.State, res.Seq)
	}
	if res.Replaced != "" {
		var old api.Message
		if c.Do(ctx, "GET", computerPath(name, "/inbox/"+url.PathEscape(res.Replaced)), nil, &old) == nil {
			fmt.Printf("replaced %s (pending since %s)\n", res.Replaced, old.CreatedAt.Local().Format("15:04"))
		} else {
			fmt.Printf("replaced %s\n", res.Replaced)
		}
	}
	return nil
}

func runInbox(g *globals, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "rm":
			return runInboxRm(g, args[1:])
		case "clear":
			return runInboxClear(g, args[1:])
		case "hold":
			return runInboxHold(g, args[1:])
		case "resume":
			return runInboxResume(g, args[1:])
		}
	}
	fs := g.flags("inbox")
	all := fs.Bool("all", false, "include delivered and ended messages")
	bodies := fs.Bool("bodies", false, "print message bodies")
	asJSON := &g.json
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("inbox needs one Computer name (or rm, clear, hold, resume)")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	q := url.Values{}
	if *all {
		q.Set("state", "all")
	}
	if *bodies {
		q.Set("bodies", "true")
	}
	var l api.InboxListResponse
	if err := c.Do(ctx, "GET", computerPath(pos[0], "/inbox?"+q.Encode()), nil, &l); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(l)
	}
	cli.WriteInbox(os.Stdout, &l, *bodies)
	return nil
}

func runInboxRm(g *globals, args []string) error {
	fs := g.flags("inbox rm")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError("inbox rm needs a Computer name and a message id")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var m api.Message
	if err := c.Do(ctx, "DELETE", computerPath(pos[0], "/inbox/"+url.PathEscape(pos[1])), nil, &m); err != nil {
		return err
	}
	if g.json {
		return printJSON(m)
	}
	fmt.Printf("withdrew %s from %s\n", m.ID, pos[0])
	return nil
}

func runInboxClear(g *globals, args []string) error {
	fs := g.flags("inbox clear")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	orphan := fs.Bool("orphan", false, "clear the inbox of a Computer no longer in the Fleet")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("inbox clear needs one Computer name")
	}
	name := pos[0]
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var l api.InboxListResponse
	if err := c.Do(ctx, "GET", computerPath(name, "/inbox"), nil, &l); err != nil {
		return err
	}
	if l.Pending == 0 {
		if g.json {
			return printJSON(api.ClearResponse{})
		}
		fmt.Printf("%s has no pending messages\n", name)
		return nil
	}
	head := int64(0)
	for _, m := range l.Messages {
		head = max(head, m.Seq)
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "discard %d pending message(s) for %s? They are journaled with their bodies, not erased. [y/N] ", l.Pending, name)
		if !confirmed(os.Stdin) {
			fmt.Fprintln(os.Stderr, "nothing cleared")
			return exitCode(1)
		}
	}
	var res api.ClearResponse
	if err := c.Do(ctx, "POST", computerPath(name, "/inbox/clear"), api.ClearRequest{ThroughSeq: &head, Orphan: *orphan}, &res); err != nil {
		return err
	}
	if g.json {
		return printJSON(res)
	}
	fmt.Printf("discarded %d message(s) for %s\n", res.Discarded, name)
	return nil
}

func runInboxHold(g *globals, args []string) error {
	fs := g.flags("inbox hold")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("inbox hold needs one Computer name")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var s api.InboxSummary
	if err := c.Do(ctx, "POST", computerPath(pos[0], "/inbox/hold"), api.HoldRequest{}, &s); err != nil {
		return err
	}
	if g.json {
		return printJSON(s)
	}
	fmt.Printf("%s's inbox is held: messages are kept, not delivered (a12s inbox resume %s)\n", pos[0], pos[0])
	return nil
}

func runInboxResume(g *globals, args []string) error {
	fs := g.flags("inbox resume")
	fresh := fs.Bool("new", false, "deliver the next Batch in a new session")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("inbox resume needs one Computer name")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	req := api.ResumeRequest{Session: api.SessionCurrent}
	if *fresh {
		req.Session = api.SessionNew
	}
	var s api.InboxSummary
	if err := c.Do(ctx, "POST", computerPath(pos[0], "/inbox/resume"), req, &s); err != nil {
		return err
	}
	if g.json {
		return printJSON(s)
	}
	msg := fmt.Sprintf("%s's inbox resumed", pos[0])
	if *fresh {
		msg += "; the next Batch starts a new session"
	}
	fmt.Println(msg)
	return nil
}

func runToken(g *globals, args []string) error {
	if len(args) == 0 {
		return usageError("token needs create, ls or rm")
	}
	sub, args := args[0], args[1:]
	fs := g.flags("token " + sub)
	actions := fs.String("actions", "", "with create: comma-separated actions ("+strings.Join(api.TokenActions, ",")+")")
	computers := fs.String("computers", "", "with create: comma-separated Computers, or *")
	asJSON := &g.json
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	split := func(s string) []string {
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	switch sub {
	case "create":
		if len(pos) != 1 {
			return usageError("token create needs a name")
		}
		var t api.Token
		if err := c.Do(ctx, "POST", "/v1/tokens", api.TokenRequest{Name: pos[0], Actions: split(*actions), Computers: split(*computers)}, &t); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(t)
		}
		fmt.Printf("token %s (actions %s on %s); shown once, store it now:\n%s\n", t.Name, strings.Join(t.Actions, ","), strings.Join(t.Computers, ","), t.Secret)
	case "ls":
		var res api.TokensResponse
		if err := c.Do(ctx, "GET", "/v1/tokens", nil, &res); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(res)
		}
		if len(res.Tokens) == 0 {
			fmt.Println("no scoped tokens")
		}
		for _, t := range res.Tokens {
			fmt.Printf("%-20s %-28s %s\n", t.Name, strings.Join(t.Actions, ","), strings.Join(t.Computers, ","))
		}
	case "rm":
		if len(pos) != 1 {
			return usageError("token rm needs a name")
		}
		if err := c.Do(ctx, "DELETE", "/v1/tokens/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			return err
		}
		if g.json {
			return printJSON(map[string]string{"revoked": pos[0]})
		}
		fmt.Printf("revoked token %s\n", pos[0])
	default:
		return usageError("token needs create, ls or rm")
	}
	return nil
}
