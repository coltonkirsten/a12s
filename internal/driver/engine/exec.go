package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// Exec runs cmd in a container and returns stdout and stderr combined; name labels errors.
func (e *Engine) Exec(ctx context.Context, containerName, name, user string, cmd []string, stdin io.Reader) (int, string, error) {
	created, err := e.cli.ExecCreate(ctx, containerName, client.ExecCreateOptions{
		User:         user,
		Cmd:          cmd,
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return -1, "", fmt.Errorf("exec %s: %w", name, err)
	}
	att, err := e.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, "", fmt.Errorf("exec %s: attach: %w", name, err)
	}
	defer att.Close()

	stdinErr := make(chan error, 1)
	if stdin != nil {
		go func() {
			_, err := io.Copy(att.Conn, stdin)
			if cerr := att.CloseWrite(); err == nil {
				err = cerr
			}
			stdinErr <- err
		}()
	} else {
		stdinErr <- nil
	}
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, att.Reader); err != nil {
		return -1, out.String(), fmt.Errorf("exec %s: read output: %w", name, err)
	}
	select {
	case err := <-stdinErr:
		if err != nil {
			return -1, out.String(), fmt.Errorf("exec %s: write stdin: %w", name, err)
		}
	case <-ctx.Done():
		return -1, out.String(), fmt.Errorf("exec %s: %w", name, ctx.Err())
	}
	// The output stream ends as the process exits; the exit code can lag a moment.
	for {
		res, err := e.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return -1, out.String(), fmt.Errorf("exec %s: inspect: %w", name, err)
		}
		if !res.Running {
			return res.ExitCode, out.String(), nil
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return -1, out.String(), fmt.Errorf("exec %s: %w", name, ctx.Err())
		}
	}
}

// Sessions counts every running exec, not just a12s shell; any keeps the Computer awake.
func (e *Engine) Sessions(ctx context.Context, containerName string) (int, error) {
	res, err := e.cli.ContainerInspect(ctx, containerName, client.ContainerInspectOptions{})
	if IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range res.Container.ExecIDs {
		if ex, err := e.cli.ExecInspect(ctx, id, client.ExecInspectOptions{}); err == nil && ex.Running {
			n++
		}
	}
	return n, nil
}
