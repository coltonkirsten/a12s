package engine

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

const (
	bundleSrc    = "/bundle"
	extractMount = "/a12s-bundle-cache"
)

var bundleNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)

// EnsureImage pulls only if the image is missing and returns its ID.
func (e *Engine) EnsureImage(ctx context.Context, ref string) (string, error) {
	res, err := e.cli.ImageInspect(ctx, ref)
	if err == nil {
		return res.ID, nil
	}
	if !IsNotFound(err) {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	pull, err := e.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return "", fmt.Errorf("pull image: %w", err)
	}
	if err := pull.Wait(ctx); err != nil {
		return "", fmt.Errorf("pull image: %w", err)
	}
	if res, err = e.cli.ImageInspect(ctx, ref); err != nil {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	return res.ID, nil
}

func (e *Engine) InspectComputerImage(ctx context.Context, imageID string) (driver.ComputerImage, error) {
	res, err := e.cli.ImageInspect(ctx, imageID)
	if err != nil {
		return driver.ComputerImage{}, fmt.Errorf("inspect image %s: %w", imageID, err)
	}
	var out driver.ComputerImage
	if res.Config != nil {
		out.Labels = res.Config.Labels
	}
	id, err := e.CreateExtractor(ctx, imageID, "")
	if err != nil {
		return driver.ComputerImage{}, fmt.Errorf("inspect image %s: %w", imageID, err)
	}
	defer e.RemoveExtractor(ctx, id)
	_, err = e.cli.ContainerStatPath(ctx, id, client.ContainerStatPathOptions{Path: driver.ComputerdMain})
	switch {
	case err == nil:
		out.Computerd = true
	case !IsNotFound(err):
		return driver.ComputerImage{}, fmt.Errorf("inspect image %s: %w", imageID, err)
	}
	return out, nil
}

// EnsureBundle extracts a bundle image's /bundle into the cache volume, under its BundleDir. It never starts the
// extractor containers, so the image needs no shell. bundle.json is written last: its presence marks a complete
// extraction.
func (e *Engine) EnsureBundle(ctx context.Context, imageRef, cacheVolume string) (driver.Bundle, error) {
	return e.EnsureBundleAs(ctx, imageRef, cacheVolume, "")
}

// EnsureBundleAs extracts under the BundleDir of dirImageID instead of the image's own ID: the ID the image has in
// another engine, the one Computers' specs name, when it was copied into this one under a different ID.
func (e *Engine) EnsureBundleAs(ctx context.Context, imageRef, cacheVolume, dirImageID string) (driver.Bundle, error) {
	imageID, err := e.EnsureImage(ctx, imageRef)
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: %w", imageRef, err)
	}
	if _, err := e.EnsureVolume(ctx, cacheVolume, map[string]string{LabelFleet: e.fleet}, LabelFleet, e.fleet); err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: %w", imageRef, err)
	}

	// Two containers: the engine locks a container while streaming an archive out,
	// so copying out of and into the same one deadlocks on large bundles.
	created, err := e.CreateExtractor(ctx, imageID, cacheVolume)
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: create extractor: %w", imageRef, err)
	}
	defer e.RemoveExtractor(ctx, created)
	source, err := e.CreateExtractor(ctx, imageID, "")
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: create extractor: %w", imageRef, err)
	}
	defer e.RemoveExtractor(ctx, source)

	m, err := e.readManifest(ctx, source)
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: %w", imageRef, err)
	}
	out := driver.Bundle{ImageID: imageID, Manifest: m}
	if dirImageID == "" {
		dirImageID = imageID
	}
	dir := driver.BundleDir(m.Kind, m.Version, dirImageID)

	_, err = e.cli.ContainerStatPath(ctx, created, client.ContainerStatPathOptions{Path: path.Join(extractMount, dir, bundleManifest)})
	if err == nil {
		return out, nil // already extracted
	}
	if !IsNotFound(err) {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: stat cache: %w", imageRef, err)
	}

	src, err := e.cli.CopyFromContainer(ctx, source, client.CopyFromContainerOptions{SourcePath: bundleSrc})
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: read /bundle: %w", imageRef, err)
	}
	defer src.Content.Close()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(rebaseBundleTar(src.Content, pw, path.Base(bundleSrc), dir)) }()
	_, err = e.cli.CopyToContainer(ctx, created, client.CopyToContainerOptions{DestinationPath: extractMount, Content: pr})
	pr.CloseWithError(err)
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: write cache: %w", imageRef, err)
	}
	return out, nil
}

// CreateExtractor creates a never-started container of imageID, with cacheVolume (if any) mounted to copy into.
func (e *Engine) CreateExtractor(ctx context.Context, imageID, cacheVolume string) (string, error) {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	host := &container.HostConfig{NetworkMode: "none"}
	if cacheVolume != "" {
		host.Mounts = []mount.Mount{VolumeMount(cacheVolume, extractMount, false)}
	}
	created, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "a12s-" + e.fleet + "-bundle-extract-" + hex.EncodeToString(suffix),
		Config: &container.Config{
			Image:      imageID,
			Entrypoint: []string{"/a12s-never-started"},
			Labels:     map[string]string{LabelFleet: e.fleet},
		},
		HostConfig: host,
	})
	return created.ID, err
}

func (e *Engine) RemoveExtractor(ctx context.Context, id string) {
	_, _ = e.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true})
}

// EnsureVolume creates the volume, or refuses an existing one not labeled for this Fleet with labels[key] == want.
func (e *Engine) EnsureVolume(ctx context.Context, name string, labels map[string]string, key, want string) (bool, error) {
	res, err := e.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil {
		got := res.Volume.Labels
		if got[LabelFleet] != e.fleet || got[key] != want {
			return false, fmt.Errorf("volume %s exists but is not owned by fleet %s", name, e.fleet)
		}
		return false, nil
	}
	if !IsNotFound(err) {
		return false, fmt.Errorf("inspect volume %s: %w", name, err)
	}
	if _, err := e.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
		return false, fmt.Errorf("create volume %s: %w", name, err)
	}
	return true, nil
}

func (e *Engine) readManifest(ctx context.Context, containerID string) (driver.BundleManifest, error) {
	var m driver.BundleManifest
	res, err := e.cli.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{SourcePath: path.Join(bundleSrc, bundleManifest)})
	if err != nil {
		return m, fmt.Errorf("read %s/%s: %w", bundleSrc, bundleManifest, err)
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	if _, err := tr.Next(); err != nil {
		return m, fmt.Errorf("read %s: %w", bundleManifest, err)
	}
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return m, fmt.Errorf("parse %s: %w", bundleManifest, err)
	}
	if !bundleNameRE.MatchString(m.Kind) || !bundleNameRE.MatchString(m.Version) {
		return m, fmt.Errorf("%s: invalid kind %q or version %q", bundleManifest, m.Kind, m.Version)
	}
	if m.Bin == "" || path.IsAbs(m.Bin) || strings.HasPrefix(path.Clean(m.Bin), "..") {
		return m, fmt.Errorf("%s: bin %q must be a relative path inside the bundle", bundleManifest, m.Bin)
	}
	return m, nil
}

// rebaseBundleTar holds bundle.json back to write it last; entries are owned by root.
func rebaseBundleTar(r io.Reader, w io.Writer, from, to string) error {
	tr, tw := tar.NewReader(r), tar.NewWriter(w)
	var manifest *tar.Header
	var manifestBody []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read bundle tar: %w", err)
		}
		rel, ok := strings.CutPrefix(h.Name, from)
		if !ok || (rel != "" && rel[0] != '/') {
			return fmt.Errorf("bundle tar: unexpected entry %q", h.Name)
		}
		h.Name = to + rel
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "root", "root"
		if rel == "/"+bundleManifest {
			manifest = h
			if manifestBody, err = io.ReadAll(io.LimitReader(tr, 1<<20)); err != nil {
				return fmt.Errorf("read bundle tar: %w", err)
			}
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("write bundle tar: %w", err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return fmt.Errorf("write bundle tar: %w", err)
		}
	}
	if manifest == nil {
		return fmt.Errorf("bundle tar: no %s", bundleManifest)
	}
	manifest.Size = int64(len(manifestBody))
	if err := tw.WriteHeader(manifest); err != nil {
		return fmt.Errorf("write bundle tar: %w", err)
	}
	if _, err := tw.Write(manifestBody); err != nil {
		return fmt.Errorf("write bundle tar: %w", err)
	}
	return tw.Close()
}
