package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
)

// TerminalProtocol is the Upgrade token of GET /v1/agent-computers/{c}/terminal: after the 101 the connection
// carries the TTY's raw bytes both ways.
const TerminalProtocol = "a12s-terminal"

// terminals are the open ones, by id, so a resize can find its TTY.
type terminals struct {
	mu   sync.Mutex
	open map[string]openTerminal
}

type openTerminal struct {
	computer string
	tty      driver.Terminal
}

func (t *terminals) add(computer string, tty driver.Terminal) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := "term_" + hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open == nil {
		t.open = map[string]openTerminal{}
	}
	t.open[id] = openTerminal{computer: computer, tty: tty}
	return id
}

func (t *terminals) get(computer, id string) (driver.Terminal, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	o, ok := t.open[id]
	return o.tty, ok && o.computer == computer
}

func (t *terminals) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.open, id)
}

func termSize(r *http.Request) (rows, cols uint, err error) {
	rows, cols = 24, 80
	for _, f := range []struct {
		key string
		v   *uint
	}{{"rows", &rows}, {"cols", &cols}} {
		if s := r.URL.Query().Get(f.key); s != "" {
			n, perr := strconv.ParseUint(s, 10, 16)
			if perr != nil || n == 0 {
				return 0, 0, errorf(http.StatusBadRequest, api.ErrInvalid, "%s must be a number from 1 to 65535", f.key)
			}
			*f.v = uint(n)
		}
	}
	return rows, cols, nil
}

// handleTerminal opens an interactive shell in a running agent computer, as its agent user in /home/agent, and
// switches the connection to it (HTTP/1.1 Upgrade). It works on every substrate: the Controller drives each
// computer's engine. The shell is journaled, counts as a session (it keeps the computer awake) and ends, with
// everything it started, when the connection closes.
func (c *Controller) handleTerminal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	if !headerHas(r.Header, "Connection", "upgrade") || !headerHas(r.Header, "Upgrade", TerminalProtocol) {
		writeJSON(w, http.StatusUpgradeRequired, api.Error{Code: api.ErrInvalid,
			Message: "a terminal is an HTTP/1.1 Upgrade",
			Fix:     "send Connection: Upgrade and Upgrade: " + TerminalProtocol})
		return
	}
	rows, cols, err := termSize(r)
	if err != nil {
		fail(w, err)
		return
	}
	cl, err := c.awake(name)
	if err != nil {
		fail(w, err)
		return
	}
	who := callerOf(r).principal
	if err := cl.Diagnostic(r.Context(), "terminal opened", map[string]any{"user": "1000:1000", "by": who}); err != nil {
		fail(w, err)
		return
	}
	tty, err := c.drv.Terminal(r.Context(), name, rows, cols)
	if err != nil {
		fail(w, err)
		return
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = tty.Close()
		writeJSON(w, http.StatusBadRequest, api.Error{Code: api.ErrInvalid, Message: "this connection can't be upgraded: " + err.Error(),
			Fix: "use HTTP/1.1"})
		return
	}
	id := c.terms.add(name, tty)
	c.logf("%s: terminal %s opened by %s", name, id, who)
	opened := time.Now()
	defer func() {
		c.terms.remove(id)
		_ = tty.Close()
		_ = conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.client(name).Diagnostic(ctx, "terminal closed", map[string]any{"by": who, "seconds": int(time.Since(opened).Seconds())})
		c.logf("%s: terminal %s closed", name, id)
	}()
	_ = conn.SetDeadline(time.Time{})
	fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\nA12s-Terminal: %s\r\n\r\n", TerminalProtocol, id)
	if err := buf.Flush(); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(tty, buf.Reader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, tty); done <- struct{}{} }()
	<-done
}

// headerHas reports whether a comma-separated header names token (case-insensitively).
func headerHas(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func (c *Controller) handleTerminalResize(w http.ResponseWriter, r *http.Request) {
	var req api.TerminalResize
	if !decode(w, r, 1<<10, &req) {
		return
	}
	if req.Rows == 0 || req.Cols == 0 || req.Rows > 65535 || req.Cols > 65535 {
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "rows and cols must be from 1 to 65535")
		return
	}
	tty, ok := c.terms.get(r.PathValue("c"), r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, api.ErrNotFound, "no open terminal "+r.PathValue("id"))
		return
	}
	if err := tty.Resize(r.Context(), req.Rows, req.Cols); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
