//go:build unix

package serviceinit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// endpoint mirrors driver.Endpoint (A12S_USES) and tcpEgress
// driver.TCPEgress (A12S_EGRESS_TCP); kept local so a12s-init stays small.
type endpoint struct {
	Service  string `json:"service"`
	Number   int    `json:"number"`
	Protocol string `json:"protocol"`
	Env      string `json:"env"`
}

type tcpEgress struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Env  string `json:"env"`
}

// wire sets the address variable of every used Service port and of every
// named TCP egress. HTTP ports are reached as http://<svc>.a12s:<port>
// through the proxy; TCP ports and egress get a local forwarder on an
// allocated loopback port, bridged by CONNECT through the proxy (§5.4). The
// forwarder is a child process at the manifest uid, holding the listeners.
func wire(uid, gid int) error {
	var uses []endpoint
	var egress []tcpEgress
	if s := os.Getenv("A12S_USES"); s != "" {
		if err := json.Unmarshal([]byte(s), &uses); err != nil {
			return fmt.Errorf("a12s-init: A12S_USES: %w", err)
		}
	}
	if s := os.Getenv("A12S_EGRESS_TCP"); s != "" {
		if err := json.Unmarshal([]byte(s), &egress); err != nil {
			return fmt.Errorf("a12s-init: A12S_EGRESS_TCP: %w", err)
		}
	}
	var files []*os.File
	var targets []string
	listen := func(env, target string) error {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("a12s-init: forwarder for %s: %w", target, err)
		}
		f, err := ln.(*net.TCPListener).File()
		ln.Close()
		if err != nil {
			return err
		}
		files = append(files, f)
		targets = append(targets, target)
		os.Setenv(env, ln.Addr().String())
		return nil
	}
	for _, e := range uses {
		if e.Protocol == "tcp" {
			if err := listen(e.Env, net.JoinHostPort(e.Service+".a12s", strconv.Itoa(e.Number))); err != nil {
				return err
			}
			continue
		}
		os.Setenv(e.Env, "http://"+e.Service+".a12s:"+strconv.Itoa(e.Number))
	}
	for _, t := range egress {
		if err := listen(t.Env, net.JoinHostPort(t.Name+".egress.a12s", strconv.Itoa(t.Port))); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, append([]string{"service-forward", os.Getenv("A12S_PROXY")}, targets...)...)
	cmd.ExtraFiles = files
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{uint32(gid)}}}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("a12s-init: start forwarders: %w", err)
	}
	for _, f := range files {
		f.Close()
	}
	return nil
}

// ServeForwards is `a12s service-forward <proxy> <target>...`: listener i
// is inherited as fd 3+i and forwards to target i through the proxy.
func ServeForwards(proxy string, targets []string, stderr io.Writer) error {
	if len(targets) == 0 {
		return errors.New("service-forward: no targets")
	}
	errc := make(chan error, len(targets))
	for i, t := range targets {
		ln, err := net.FileListener(os.NewFile(uintptr(3+i), "listener"))
		if err != nil {
			return fmt.Errorf("service-forward: %w", err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					errc <- err
					return
				}
				go func() {
					defer c.Close()
					u, err := DialThroughProxy(proxy, t)
					if err != nil {
						fmt.Fprintf(stderr, "a12s forwarder: %s: %v\n", t, err)
						return
					}
					defer u.Close()
					pipe(c, u)
				}()
			}
		}()
	}
	return <-errc
}

// DialThroughProxy opens a CONNECT tunnel to target through an HTTP proxy.
func DialThroughProxy(proxy, target string) (net.Conn, error) {
	pu, err := url.Parse(proxy)
	if err != nil || pu.Host == "" {
		return nil, fmt.Errorf("proxy %q: not a URL", proxy)
	}
	c, err := net.DialTimeout("tcp", pu.Host, 10*time.Second)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		c.Close()
		return nil, fmt.Errorf("proxy refused: %s %s", res.Status, strings.TrimSpace(string(b)))
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: c, r: br}, nil
	}
	return c, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(b, a); closeWrite(b); done <- struct{}{} }()
	go func() { io.Copy(a, b); closeWrite(a); done <- struct{}{} }()
	<-done
	<-done
}
