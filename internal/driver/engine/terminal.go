package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// TerminalShell starts bash where the image has it, else sh, as a login shell.
var TerminalShell = []string{"sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi"}

// Terminal is an interactive shell on a TTY inside a container: reads are the TTY's output, writes its input.
type Terminal struct {
	e  *Engine
	id string
	// mark is in the environment of every process the shell starts, so Close can end them all.
	mark string
	user string
	conn net.Conn
	r    io.Reader
	once sync.Once
}

// Terminal starts cmd on a TTY of rows×cols in the container and attaches to it.
func (e *Engine) Terminal(ctx context.Context, containerName, user, workdir string, cmd []string, rows, cols uint) (*Terminal, error) {
	size := client.ConsoleSize{Height: rows, Width: cols}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	mark := hex.EncodeToString(b)
	created, err := e.cli.ExecCreate(ctx, containerName, client.ExecCreateOptions{
		User:         user,
		TTY:          true,
		ConsoleSize:  size,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          []string{"TERM=xterm-256color", "HOME=" + AgentHome, "A12S_TERMINAL=" + mark},
		WorkingDir:   workdir,
		Cmd:          cmd,
	})
	if err != nil {
		return nil, fmt.Errorf("terminal in %s: %w", containerName, err)
	}
	att, err := e.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if err != nil {
		return nil, fmt.Errorf("terminal in %s: attach: %w", containerName, err)
	}
	return &Terminal{e: e, id: created.ID, mark: mark, user: user, conn: att.Conn, r: att.Reader}, nil
}

func (t *Terminal) Read(p []byte) (int, error)  { return t.r.Read(p) }
func (t *Terminal) Write(p []byte) (int, error) { return t.conn.Write(p) }

// Resize changes the TTY's size.
func (t *Terminal) Resize(ctx context.Context, rows, cols uint) error {
	_, err := t.e.cli.ExecResize(ctx, t.id, client.ExecResizeOptions{Height: rows, Width: cols})
	return err
}

// Close detaches and ends the shell with everything it started. A TTY exec outlives its attach, and a running exec
// keeps the Computer awake, so every process carrying the terminal's mark gets SIGHUP, then SIGKILL, from the
// shell's own user (root in a container may not read another user's /proc/<pid>/environ).
func (t *Terminal) Close() error {
	var err error
	t.once.Do(func() {
		err = t.conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, ierr := t.e.cli.ExecInspect(ctx, t.id, client.ExecInspectOptions{})
		if ierr != nil || !res.Running {
			return
		}
		script := `for sig in HUP KILL; do for p in /proc/[0-9]*; do tr '\0' '\n' < $p/environ 2>/dev/null | grep -qx "A12S_TERMINAL=$1" && kill -$sig ${p#/proc/} 2>/dev/null; done; sleep 2; done`
		_, _, _ = t.e.Exec(ctx, res.ContainerID, "terminal", t.user, []string{"sh", "-c", script, "sh", t.mark}, nil)
	})
	return err
}
