//go:build unix

// Package serviceinit is a12s-init (enhancements §5.5): the a12s binary,
// mounted read-only into every Service, run as its entrypoint. It does the
// privileged part of the start and nothing else: it prepares the state and
// tmpfs mounts for the manifest uid, waits for the Controller's injection,
// exports the Service's config env, then drops to the manifest uid (every
// capability goes with uid 0) and execs the image's own program.
//
// It also carries the two other in-container helpers the Controller needs
// without a shell in the image: WriteFile (the injection write) and
// Forward (a Service's publish forwarder).
package serviceinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Paths inside a Service.
const (
	RunDir         = "/run/a12s"
	SecretsDir     = RunDir + "/secrets"
	ConfigDir      = RunDir + "/config"
	ConfigFile     = ConfigDir + "/config.json"
	TLSDir         = RunDir + "/tls"
	GenerationFile = SecretsDir + "/.generation"
)

// Env is what a12s-init reads (set by the Driver).
const (
	EnvUser  = "A12S_SERVICE_USER" // "uid:gid"
	EnvState = "A12S_STATE_AT"
	EnvTmpfs = "A12S_TMPFS" // comma-separated
)

// BarrierTimeout bounds the wait for the Controller's injection.
var BarrierTimeout = 5 * time.Minute

// Init runs a12s-init: args is the image's program and its arguments.
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

// prepareState hands an empty state mount to the manifest uid. A mount
// that already has content keeps its ownership (it is the Service's data).
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

// exportConfigEnv sets each config.env entry in the environment (strings
// as they are; anything else as JSON).
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

// drop switches to uid:gid with no supplementary groups. Leaving uid 0
// clears every capability; no-new-privileges keeps them from coming back.
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

// WriteFile is the Controller's injection write: stdin to path, owned by
// owner ("uid:gid") with mode, in a root-owned directory with dirMode. The
// bytes go to a temporary file first and are renamed over path, so a
// reader never sees a partial file and nothing is written through an
// existing path.
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

// Forward runs a publish forwarder: each "<port>=<host>:<port>" listens on
// the first port and forwards every connection to the target. It never
// returns unless a listener fails.
func Forward(specs []string, stderr io.Writer) error {
	if len(specs) == 0 {
		return errors.New("publish-forward: nothing to forward")
	}
	errc := make(chan error, len(specs))
	for _, s := range specs {
		port, target, ok := strings.Cut(s, "=")
		if _, err := strconv.Atoi(port); !ok || err != nil {
			return fmt.Errorf("publish-forward: %q: want <port>=<host>:<port>", s)
		}
		ln, err := net.Listen("tcp", ":"+port)
		if err != nil {
			return fmt.Errorf("publish-forward: %w", err)
		}
		go func() { errc <- serve(ln, target, stderr) }()
	}
	return <-errc
}

func serve(ln net.Listener, target string, stderr io.Writer) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			u, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				fmt.Fprintf(stderr, "publish-forward: %s: %v\n", target, err)
				return
			}
			defer u.Close()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); io.Copy(u, c); closeWrite(u) }()
			go func() { defer wg.Done(); io.Copy(c, u); closeWrite(c) }()
			wg.Wait()
		}()
	}
}

func closeWrite(c net.Conn) {
	if b, ok := c.(*bufferedConn); ok {
		c = b.Conn
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// LoginHold is a login container's process: it makes HOME, XDG_* and the
// output directory on the tmpfs and waits to be removed.
func LoginHold() error {
	for _, d := range []string{"home", "config", "cache", "data", "state", "out"} {
		if err := os.MkdirAll(filepath.Join(RunDir, "login", d), 0o700); err != nil {
			return err
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	return nil
}

// LoginRead prints what the login command wrote to $A12S_LOGIN_OUT.
func LoginRead(w io.Writer) error {
	p := filepath.Join(RunDir, "login", "out", "credential")
	data, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("the login command wrote nothing to $A12S_LOGIN_OUT (%s): %w", p, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("$A12S_LOGIN_OUT (%s) is empty", p)
	}
	_, err = w.Write(data)
	return err
}
