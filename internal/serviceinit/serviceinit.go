//go:build unix

// Package serviceinit is a12s-init, every Service's entrypoint. It does only the
// privileged part of the start, then drops to the manifest uid before exec.
package serviceinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	RunDir         = "/run/a12s"
	SecretsDir     = RunDir + "/secrets"
	ConfigDir      = RunDir + "/config"
	ConfigFile     = ConfigDir + "/config.json"
	TLSDir         = RunDir + "/tls"
	GenerationFile = SecretsDir + "/.generation"
)

const (
	EnvUser  = "A12S_SERVICE_USER" // "uid:gid"
	EnvState = "A12S_STATE_AT"
	EnvTmpfs = "A12S_TMPFS" // comma-separated
)

var BarrierTimeout = 5 * time.Minute

func Init(args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("a12s-init: no program to run (the image has no entrypoint or command)")
	}
	uid, gid, err := parseUser(os.Getenv(EnvUser))
	if err != nil {
		return err
	}
	if st := os.Getenv(EnvState); st != "" {
		if err := prepareState(st, uid, gid); err != nil {
			return err
		}
	}
	for _, t := range splitList(os.Getenv(EnvTmpfs)) {
		if err := os.MkdirAll(t, 0o700); err != nil {
			return fmt.Errorf("a12s-init: tmpfs %s: %w", t, err)
		}
		if err := os.Chown(t, uid, gid); err != nil {
			return fmt.Errorf("a12s-init: tmpfs %s: %w", t, err)
		}
	}
	if err := waitFile(GenerationFile, BarrierTimeout); err != nil {
		return err
	}
	if err := exportConfigEnv(ConfigFile); err != nil {
		return err
	}
	if err := wire(uid, gid); err != nil {
		return err
	}
	if h := os.Getenv("HOME"); h == "" || h == "/root" {
		os.Setenv("HOME", "/tmp")
	}
	if err := drop(uid, gid); err != nil {
		return err
	}
	prog, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("a12s-init: %w", err)
	}
	return syscall.Exec(prog, args, os.Environ())
}

func parseUser(s string) (int, int, error) {
	us, gs, ok := strings.Cut(s, ":")
	uid, err1 := strconv.Atoi(us)
	gid, err2 := strconv.Atoi(gs)
	if !ok || err1 != nil || err2 != nil || uid <= 0 || gid < 0 {
		return 0, 0, fmt.Errorf("a12s-init: %s=%q: want a non-root \"uid:gid\"", EnvUser, s)
	}
	return uid, gid, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// prepareState chowns only an empty mount; existing content is the Service's data.
func prepareState(dir string, uid, gid int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("a12s-init: state %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return nil
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("a12s-init: state %s: %w", dir, err)
	}
	return os.Chmod(dir, 0o700)
}

func waitFile(p string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("a12s-init: the Controller did not inject %s within %s", p, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func exportConfigEnv(p string) error {
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("a12s-init: %w", err)
	}
	var cfg struct {
		Env map[string]any `json:"env"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("a12s-init: %s: %w", p, err)
	}
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("a12s-init: config env key %q is not a variable name", k)
		}
		var v string
		switch x := cfg.Env[k].(type) {
		case string:
			v = x
		default:
			b, _ := json.Marshal(x)
			v = string(b)
		}
		os.Setenv(k, v)
	}
	return nil
}

// Leaving uid 0 clears every capability; no-new-privileges keeps them from coming back.
func drop(uid, gid int) error {
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("a12s-init: setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("a12s-init: setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("a12s-init: setuid: %w", err)
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return errors.New("a12s-init: still root after setuid; refusing to run the image as root")
	}
	return nil
}

// WriteFile renames a temp file over path so a reader never sees a partial
// file and nothing is written through an existing path (symlink).
func WriteFile(path, owner, mode, dirMode string, stdin io.Reader) error {
	uid, gid, err := parseOwner(owner)
	if err != nil {
		return err
	}
	m, err1 := strconv.ParseUint(mode, 8, 32)
	dm, err2 := strconv.ParseUint(dirMode, 8, 32)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("write %s: bad mode %q or %q", path, mode, dirMode)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, RunDir+"/") {
		return fmt.Errorf("write %s: only paths under %s", path, RunDir)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, os.FileMode(dm)); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("write %s: %s is not a directory", path, dir)
	}
	if err := os.Chown(dir, 0, 0); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(dir, os.FileMode(dm)); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, ".a12s-write.*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, stdin); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chown(uid, gid); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(os.FileMode(m)); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Rename(tmp.Name(), path)
}

func parseOwner(s string) (int, int, error) {
	us, gs, ok := strings.Cut(s, ":")
	uid, err1 := strconv.Atoi(us)
	gid, err2 := strconv.Atoi(gs)
	if !ok || err1 != nil || err2 != nil || uid < 0 || gid < 0 {
		return 0, 0, fmt.Errorf("owner %q: want \"uid:gid\"", s)
	}
	return uid, gid, nil
}

func closeWrite(c net.Conn) {
	if b, ok := c.(*bufferedConn); ok {
		c = b.Conn
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}
