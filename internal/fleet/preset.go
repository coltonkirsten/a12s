package fleet

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// AgentUID and AgentGID own everything a preset puts in the agent's home.
const (
	AgentUID = 1000
	AgentGID = 1000
)

// presetEntry is one normalized file in a preset directory. Modes are
// normalized (0755 or 0644) so the hash does not depend on the umask of the
// machine that checked out the repo.
type presetEntry struct {
	rel    string // slash-separated, relative to the preset root
	typ    byte   // tar.TypeDir, tar.TypeReg or tar.TypeSymlink
	mode   int64
	size   int64
	target string // symlink target
	abs    string // path on disk, for regular files
}

func walkPreset(dir string) ([]presetEntry, error) {
	var entries []presetEntry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := presetEntry{rel: rel, abs: p, mode: 0o644}
		switch {
		case d.IsDir():
			e.typ, e.mode = tar.TypeDir, 0o755
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			resolved := path.Join(path.Dir(rel), filepath.ToSlash(target))
			if path.IsAbs(target) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("preset symlink %s -> %s leaves the preset", rel, target)
			}
			e.typ, e.target = tar.TypeSymlink, target
		case d.Type().IsRegular():
			e.typ, e.size = tar.TypeReg, info.Size()
			if info.Mode()&0o111 != 0 {
				e.mode = 0o755
			}
		default:
			return fmt.Errorf("preset entry %s: unsupported file type %s", rel, d.Type())
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk preset %s: %w", dir, err)
	}
	return entries, nil
}

// HashPreset returns a deterministic content hash ("sha256:<hex>") of a preset
// directory: paths, types, normalized modes, symlink targets and file bytes.
// Timestamps and ownership are ignored.
func HashPreset(dir string) (string, error) {
	entries, err := walkPreset(dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, e := range entries {
		switch e.typ {
		case tar.TypeDir:
			fmt.Fprintf(h, "d %q %o\n", e.rel, e.mode)
		case tar.TypeSymlink:
			fmt.Fprintf(h, "l %q %q\n", e.rel, e.target)
		case tar.TypeReg:
			sum, err := fileSHA256(e.abs)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "f %q %o %d %s\n", e.rel, e.mode, e.size, sum)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("hash preset: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash preset %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WritePresetTar writes the preset directory as a tar stream whose entries are
// relative to the agent's home and owned by the agent user, with normalized
// modes and zero timestamps. It covers exactly what HashPreset hashes.
func WritePresetTar(dir string, w io.Writer) error {
	entries, err := walkPreset(dir)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{
			Typeflag: e.typ,
			Name:     e.rel,
			Linkname: e.target,
			Mode:     e.mode,
			Size:     e.size,
			Uid:      AgentUID,
			Gid:      AgentGID,
			ModTime:  time.Unix(0, 0),
			Format:   tar.FormatPAX,
		}
		if e.typ == tar.TypeDir {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("preset tar: %w", err)
		}
		if e.typ == tar.TypeReg {
			if err := copyFile(tw, e.abs, e.size); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("preset tar: %w", err)
	}
	return nil
}

func copyFile(w io.Writer, p string, size int64) error {
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("preset tar: %w", err)
	}
	defer f.Close()
	if _, err := io.CopyN(w, f, size); err != nil {
		return fmt.Errorf("preset tar %s: %w", p, err)
	}
	return nil
}
