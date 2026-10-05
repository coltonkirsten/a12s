package docker

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
	bundleSrc      = "/bundle"
	bundleManifest = "bundle.json"
	extractMount   = "/a12s-bundle-cache"
)

var bundleNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)

// EnsureBundle extracts a HarnessBundle image's /bundle into the Fleet's
// bundle cache volume as <kind>-<version>/. A throwaway container is created
// from the bundle image (never started, so the image needs no shell) with the
// cache volume mounted, and the files move through the archive API.
// bundle.json is written last, so its presence marks a complete extraction.
func (d *Driver) EnsureBundle(ctx context.Context, imageRef string) (driver.Bundle, error) {
	imageID, err := d.ensureImage(ctx, imageRef)
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: %w", imageRef, err)
	}
	if _, err := d.ensureBundleVolume(ctx); err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: %w", imageRef, err)
	}

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "a12s-" + d.fleet + "-bundle-extract-" + hex.EncodeToString(suffix),
		Config: &container.Config{
			Image:      imageID,
			Entrypoint: []string{"/a12s-never-started"},
			Labels:     map[string]string{LabelFleet: d.fleet},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: "none",
			Mounts: []mount.Mount{{
				Type: mount.TypeVolume, Source: d.BundleVolume(), Target: extractMount,
				VolumeOptions: &mount.VolumeOptions{NoCopy: true},
			}},
		},
	})
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: create extractor: %w", imageRef, err)
	}
	defer func() {
		_, _ = d.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	m, err := d.readManifest(ctx, created.ID)
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: %w", imageRef, err)
	}
	out := driver.Bundle{ImageID: imageID, Manifest: m}
	dir := driver.BundleDir(m.Kind, m.Version)

	_, err = d.cli.ContainerStatPath(ctx, created.ID, client.ContainerStatPathOptions{Path: path.Join(extractMount, dir, bundleManifest)})
	if err == nil {
		return out, nil // already extracted
	}
	if !isNotFound(err) {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: stat cache: %w", imageRef, err)
	}

	src, err := d.cli.CopyFromContainer(ctx, created.ID, client.CopyFromContainerOptions{SourcePath: bundleSrc})
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: read /bundle: %w", imageRef, err)
	}
	defer src.Content.Close()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(rebaseBundleTar(src.Content, pw, path.Base(bundleSrc), dir)) }()
	_, err = d.cli.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: extractMount, Content: pr})
	pr.CloseWithError(err)
	if err != nil {
		return driver.Bundle{}, fmt.Errorf("ensure bundle %s: write cache: %w", imageRef, err)
	}
	return out, nil
}

func (d *Driver) ensureBundleVolume(ctx context.Context) (bool, error) {
	return d.ensureVolume(ctx, d.BundleVolume(), map[string]string{LabelFleet: d.fleet}, LabelFleet, d.fleet)
}

func (d *Driver) readManifest(ctx context.Context, containerID string) (driver.BundleManifest, error) {
	var m driver.BundleManifest
	res, err := d.cli.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{SourcePath: path.Join(bundleSrc, bundleManifest)})
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

// rebaseBundleTar copies a tar stream rooted at from/ to one rooted at to/,
// holding bundle.json back to write it last. Entries are owned by root.
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

// ensureImage returns the local image ID for ref, pulling it if missing.
func (d *Driver) ensureImage(ctx context.Context, ref string) (string, error) {
	res, err := d.cli.ImageInspect(ctx, ref)
	if err == nil {
		return res.ID, nil
	}
	if !isNotFound(err) {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	pull, err := d.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return "", fmt.Errorf("pull image: %w", err)
	}
	if err := pull.Wait(ctx); err != nil {
		return "", fmt.Errorf("pull image: %w", err)
	}
	if res, err = d.cli.ImageInspect(ctx, ref); err != nil {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	return res.ID, nil
}
