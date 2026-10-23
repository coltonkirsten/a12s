package controller

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
)

// Files are read and written by a shell in the agent computer, as its agent user: the API sees exactly what the
// agent can, and writes files the agent owns. POSIX sh, stat and head only, so any image with a shell works.

// readScript lists a folder ("dir", then one kind/size/mtime/name line per entry) or reads a regular file ("file",
// its size/mtime, then at most $2 bytes). Exit 2: no such path; 3: not readable; 4: not a regular file or folder.
const readScript = `p=$1
[ -e "$p" ] || [ -L "$p" ] || exit 2
if [ -d "$p" ]; then
  [ -r "$p" ] && [ -x "$p" ] || exit 3
  cd -- "$p" || exit 3
  echo dir
  for f in .* *; do
    case $f in .|..) continue;; esac
    [ -e "$f" ] || [ -L "$f" ] || continue
    stat -c '%F/%s/%Y/%n' -- "$f" 2>/dev/null
  done
  exit 0
fi
[ -f "$p" ] || exit 4
[ -r "$p" ] || exit 3
echo file
stat -L -c '%s/%Y' -- "$p" || exit 3
head -c "$2" -- "$p"`

// writeFileScript replaces $1 with stdin, making its folder first.
const writeFileScript = `mkdir -p -- "$(dirname -- "$1")" && cat > "$1"`

// filePath is the ?path= of a files call: absolute and clean; none is the agent's home.
func filePath(r *http.Request) (string, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return engine.AgentHome, nil
	}
	if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsRune(p, 0) {
		return "", errorf(http.StatusBadRequest, api.ErrInvalid, "path %q must be absolute and clean, like /home/agent/notes.md", p)
	}
	return p, nil
}

func fileExitError(code int, p, out string) error {
	switch code {
	case 2:
		return errorf(http.StatusNotFound, api.ErrNotFound, "no such file or folder: %s", p)
	case 3:
		return errorf(http.StatusForbidden, api.ErrForbidden, "the agent user can't read %s", p)
	case 4:
		return errorf(http.StatusBadRequest, api.ErrInvalid, "%s is not a regular file or folder", p)
	}
	return errorf(http.StatusInternalServerError, api.ErrInternal, "reading %s: exit %d: %s", p, code, strings.TrimSpace(out))
}

func (c *Controller) handleFileGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	p, err := filePath(r)
	if err != nil {
		fail(w, err)
		return
	}
	if _, err := c.awake(name); err != nil {
		fail(w, err)
		return
	}
	code, out, err := c.drv.Exec(r.Context(), name, []string{"sh", "-c", readScript, "sh", p, strconv.Itoa(api.MaxFileRead)}, nil, false)
	if err != nil {
		fail(w, err)
		return
	}
	if code != 0 {
		fail(w, fileExitError(code, p, out))
		return
	}
	res, err := parseFileRead(p, out)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func parseFileRead(p, out string) (api.FileResponse, error) {
	kind, rest, _ := strings.Cut(out, "\n")
	switch kind {
	case "dir":
		res := api.FileResponse{Path: p, Kind: "dir", Entries: []api.FileEntry{}}
		for _, line := range strings.Split(rest, "\n") {
			f := strings.SplitN(line, "/", 4)
			if len(f) != 4 || f[3] == "" {
				continue
			}
			size, _ := strconv.ParseInt(f[1], 10, 64)
			res.Entries = append(res.Entries, api.FileEntry{Name: f[3], Kind: entryKind(f[0]), Size: size, ModTime: unixTime(f[2])})
		}
		sort.SliceStable(res.Entries, func(i, j int) bool {
			if di, dj := res.Entries[i].Kind == "dir", res.Entries[j].Kind == "dir"; di != dj {
				return di
			}
			return res.Entries[i].Name < res.Entries[j].Name
		})
		return res, nil
	case "file":
		meta, content, _ := strings.Cut(rest, "\n")
		sizeS, mtime, _ := strings.Cut(meta, "/")
		size, err := strconv.ParseInt(sizeS, 10, 64)
		if err != nil {
			return api.FileResponse{}, errorf(http.StatusInternalServerError, api.ErrInternal, "reading %s: unexpected stat output %q", p, meta)
		}
		res := api.FileResponse{Path: p, Kind: "file", Size: size, ModTime: unixTime(mtime), Truncated: int64(len(content)) < size}
		text := content
		if res.Truncated {
			// A cut can split the last character.
			for i := 0; i < utf8.UTFMax && len(text) > 0 && !utf8.ValidString(text); i++ {
				text = text[:len(text)-1]
			}
		}
		if utf8.ValidString(text) && !bytes.ContainsRune([]byte(text), 0) {
			res.Encoding, res.Content = "utf-8", text
		} else {
			res.Encoding, res.Content = "base64", base64.StdEncoding.EncodeToString([]byte(content))
		}
		return res, nil
	}
	return api.FileResponse{}, errorf(http.StatusInternalServerError, api.ErrInternal, "reading %s: unexpected output %q", p, kind)
}

func entryKind(stat string) string {
	switch stat {
	case "regular file", "regular empty file":
		return "file"
	case "directory":
		return "dir"
	case "symbolic link":
		return "link"
	}
	return "other"
}

func unixTime(s string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(n, 0).UTC().Format(time.RFC3339)
}

// handleFilePut replaces one file with the request body, as the agent user. Journaled.
func (c *Controller) handleFilePut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	p, err := filePath(r)
	if err != nil {
		fail(w, err)
		return
	}
	if p == engine.AgentHome {
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "name the file: ?path=/home/agent/…")
		return
	}
	cl, err := c.awake(name)
	if err != nil {
		fail(w, err)
		return
	}
	var body bytes.Buffer
	if _, err := body.ReadFrom(http.MaxBytesReader(w, r.Body, api.MaxFileWrite)); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, api.ErrBodyTooLarge, "a file is at most 8 MiB here: "+err.Error())
		return
	}
	size := body.Len()
	code, out, err := c.drv.Exec(r.Context(), name, []string{"sh", "-c", writeFileScript, "sh", p}, &body, false)
	if err != nil {
		fail(w, err)
		return
	}
	if code != 0 {
		writeError(w, http.StatusForbidden, api.ErrForbidden, "the agent user can't write "+p+": "+strings.TrimSpace(out))
		return
	}
	_ = cl.Diagnostic(r.Context(), "file written", map[string]any{"path": p, "bytes": size, "by": callerOf(r).principal})
	c.logf("%s: %s wrote %s (%d bytes)", name, callerOf(r).principal, p, size)
	w.WriteHeader(http.StatusNoContent)
}
