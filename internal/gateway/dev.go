package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

var (
	ansi     = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	localURL = regexp.MustCompile(`(https?)://(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1?\]):([0-9]{2,5})`)
)

// DevURL finds the local URL a dev server prints when it is ready ("Local: http://localhost:5173/").
func DevURL(line string) (*url.URL, bool) {
	m := localURL.FindStringSubmatch(ansi.ReplaceAllString(line, ""))
	if m == nil {
		return nil, false
	}
	return &url.URL{Scheme: m[1], Host: net.JoinHostPort("localhost", m[3])}, true
}

// DevServer is a dev server command the gateway started and proxies to.
type DevServer struct {
	URL  *url.URL
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// StartDev runs argv with its output on out and returns once the server is ready: when it prints a local URL, or,
// with port > 0, when that port accepts connections. It fails if the command exits first.
func StartDev(ctx context.Context, argv []string, port int, out io.Writer) (*DevServer, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	// Its own process group, so stopping it stops what it spawned too (npm run dev runs vite as a child). No stdin:
	// reading the terminal from a background process group would suspend it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the dev server: %w", err)
	}
	d := &DevServer{cmd: cmd, done: make(chan struct{})}
	go func() {
		d.err = cmd.Wait()
		_ = pw.Close()
		close(d.done)
	}()
	found := make(chan *url.URL, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			fmt.Fprintln(out, sc.Text())
			if u, ok := DevURL(sc.Text()); ok && port == 0 {
				select {
				case found <- u:
				default:
				}
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	if port > 0 {
		go func() {
			addr := net.JoinHostPort("localhost", strconv.Itoa(port))
			for {
				if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
					_ = c.Close()
					found <- &url.URL{Scheme: "http", Host: addr}
					return
				}
				select {
				case <-d.done:
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
		}()
	}
	select {
	case u := <-found:
		d.URL = u
		return d, nil
	case <-d.done:
		return nil, fmt.Errorf("the dev server exited before it was ready (%v)", d.err)
	case <-time.After(2 * time.Minute):
		d.Stop()
		return nil, errors.New("the dev server printed no http://localhost:<port> URL in 2 minutes; pass --dev-port <port>")
	case <-ctx.Done():
		d.Stop()
		return nil, ctx.Err()
	}
}

// Done is closed when the dev server exits.
func (d *DevServer) Done() <-chan struct{} { return d.done }

// Err is why it exited, once Done is closed.
func (d *DevServer) Err() error { return d.err }

// Stop ends the dev server and everything it started: SIGTERM, then SIGKILL after 5 seconds.
func (d *DevServer) Stop() {
	pgid := -d.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-d.done
	}
}
