package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// FilesMarker names the files the home drive holds. It sits outside the agent's home and is root-owned so the agent
// can't forge it.
const FilesMarker = "/home/.a12s-files"

const bundleManifest = "bundle.json"

// CreateComputer creates the container stopped, checks that its bundle is in the cache, and copies its files onto
// the home drive. A container that fails a check is removed, so a retry starts over.
func (e *Engine) CreateComputer(ctx context.Context, spec driver.ComputerSpec, p Placement) (string, error) {
	created, err := e.cli.ContainerCreate(ctx, e.CreateOptions(spec, p))
	if err != nil {
		return "", fmt.Errorf("create container: %w", ExplainMountError(err, spec.Attach))
	}
	if err := e.CheckBundle(ctx, created.ID, spec); err != nil {
		return "", e.Discard(ctx, created.ID, err)
	}
	if len(spec.Files) > 0 {
		if err := e.applyFiles(ctx, created.ID, spec); err != nil {
			return "", e.Discard(ctx, created.ID, err)
		}
	}
	return created.ID, nil
}

// Discard reports a failed removal rather than assuming the container is gone.
func (e *Engine) Discard(ctx context.Context, containerID string, cause error) error {
	if _, err := e.cli.ContainerRemove(context.WithoutCancel(ctx), containerID, client.ContainerRemoveOptions{Force: true}); err != nil && !IsNotFound(err) {
		return errors.Join(cause, fmt.Errorf("remove half-staged container: %w", err))
	}
	return cause
}

// Bootstrapped reports whether a container CreateComputer made for spec finished: its bundle is there and its files
// are applied.
func (e *Engine) Bootstrapped(ctx context.Context, containerID string, spec driver.ComputerSpec) (bool, error) {
	if err := e.CheckBundle(ctx, containerID, spec); err != nil {
		return false, nil
	}
	if len(spec.Files) == 0 {
		return true, nil
	}
	state, err := e.ReadFile(ctx, containerID, FilesMarker)
	if err != nil {
		return false, fmt.Errorf("files: %w", err)
	}
	return state == appliedMarker(spec.FilesDigest), nil
}

func (e *Engine) CheckBundle(ctx context.Context, containerID string, spec driver.ComputerSpec) error {
	p := path.Join(spec.Bundle.Path(), bundleManifest)
	_, err := e.cli.ContainerStatPath(ctx, containerID, client.ContainerStatPathOptions{Path: p})
	if IsNotFound(err) {
		return fmt.Errorf("bundle %s@%s (%s) is not in the cache; EnsureBundle first", spec.Bundle.Kind, spec.Bundle.Version, spec.Bundle.Image)
	}
	if err != nil {
		return fmt.Errorf("check bundle: %w", err)
	}
	return nil
}

// appliedMarker names the files a home drive holds. The marker reads "pending" during a copy, so an interrupted copy
// is retried.
func appliedMarker(digest string) string { return "applied " + digest }

// applyFiles copies the files onto the home drive unless this digest is already there. It overwrites those files
// and leaves everything else the agent wrote.
func (e *Engine) applyFiles(ctx context.Context, containerID string, spec driver.ComputerSpec) error {
	state, err := e.ReadFile(ctx, containerID, FilesMarker)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	if state == appliedMarker(spec.FilesDigest) {
		return nil
	}
	if err := e.writeMarker(ctx, containerID, "pending"); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := rebaseFilesTar(bytes.NewReader(spec.Files), &buf, path.Base(AgentHome)); err != nil {
		return err
	}
	if _, err := e.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: HomeMount, Content: &buf}); err != nil {
		return fmt.Errorf("files: copy: %w", err)
	}
	return e.writeMarker(ctx, containerID, appliedMarker(spec.FilesDigest))
}

func (e *Engine) writeMarker(ctx context.Context, containerID, state string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: path.Base(FilesMarker), Mode: 0o600, Size: int64(len(state))})
	_, _ = tw.Write([]byte(state))
	_ = tw.Close()
	if _, err := e.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: path.Dir(FilesMarker), Content: &buf}); err != nil {
		return fmt.Errorf("files: write marker: %w", err)
	}
	return nil
}

// ReadFile returns "" for a missing file and reads at most 4 KiB.
func (e *Engine) ReadFile(ctx context.Context, containerID, p string) (string, error) {
	res, err := e.cli.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{SourcePath: p})
	if IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	if _, err := tr.Next(); err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	b, err := io.ReadAll(io.LimitReader(tr, 4096))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	return string(b), nil
}

// rebaseFilesTar rejects entries that would escape dir.
func rebaseFilesTar(r io.Reader, w io.Writer, dir string) error {
	tr, tw := tar.NewReader(r), tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, Uid: agentUID, Gid: agentGID}); err != nil {
		return fmt.Errorf("files tar: %w", err)
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("files tar: %w", err)
		}
		clean := path.Clean("/" + h.Name)
		if clean == "/" {
			continue
		}
		if h.Typeflag == tar.TypeLink {
			return fmt.Errorf("files tar: hard link %q not supported", h.Name)
		}
		h.Name = dir + clean
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = agentUID, agentGID, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("files tar: %w", err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return fmt.Errorf("files tar: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("files tar: %w", err)
	}
	return nil
}
