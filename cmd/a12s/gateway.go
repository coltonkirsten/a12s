package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/gateway"
)

func runGateway(g *globals, args []string) error {
	flags := g.flags("gateway")
	port := flags.Int("port", 7012, "port on 127.0.0.1")
	web := flags.String("web", "", "directory of your web app to serve (default: the built-in dashboard)")
	dev := flags.String("dev", "", "URL of a running dev server to proxy everything but /v1/ and /a12s.js to")
	devPort := flags.Int("dev-port", 0, "the port of the dev server started after --, when it doesn't print its URL")
	allow := flags.String("allow", "status,events,send,inbox", "API actions the page may use ("+strings.Join(api.TokenActions, ",")+")")
	computers := flags.String("computers", "*", "Computers the page may reach, comma-separated")
	noOpen := flags.Bool("no-open", false, "don't open the browser")
	pos, err := parse(flags, args)
	if err != nil {
		return err
	}
	if (*web != "") && (*dev != "" || len(pos) > 0) || (*dev != "" && len(pos) > 0) {
		return usageError("pick one of --web <dir>, --dev <url> or -- <dev server command>")
	}
	if *devPort != 0 && len(pos) == 0 {
		return usageError("--dev-port is the port of the dev server command after --")
	}
	var devURL *url.URL
	if *dev != "" {
		if devURL, err = url.Parse(*dev); err != nil || (devURL.Scheme != "http" && devURL.Scheme != "https") || devURL.Host == "" {
			return usageError("--dev " + *dev + " is not a URL like http://localhost:5173")
		}
	}
	var site fs.FS
	if *web != "" {
		if st, err := os.Stat(*web); err != nil || !st.IsDir() {
			return fmt.Errorf("--web %s is not a directory", *web)
		}
		site = os.DirFS(*web)
	}
	c, f, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()

	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return fmt.Errorf("%w (pick another with --port)", err)
	}

	// The page gets a scoped token of its own, never the owner's: the Controller enforces --allow and --computers.
	// One per gateway, named for its port, so gateways on the same Fleet never revoke each other's.
	tokenName := gatewayTokenPrefix + strconv.Itoa(*port)
	var existing api.TokensResponse
	if err := c.Do(ctx, "GET", "/v1/tokens", nil, &existing); err == nil {
		for _, name := range staleGatewayTokens(existing.Tokens, *port, portInUse) {
			_ = c.Do(ctx, "DELETE", "/v1/tokens/"+name, nil, nil)
		}
	}
	var tok api.Token
	if err := c.Do(ctx, "POST", "/v1/tokens", api.TokenRequest{Name: tokenName, Actions: list(*allow), Computers: list(*computers)}, &tok); err != nil {
		return fmt.Errorf("create the gateway's token: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Do(ctx, "DELETE", "/v1/tokens/"+tokenName, nil, nil)
	}()
	serving := "the built-in dashboard"
	if *web != "" {
		serving = *web
	}
	var devExited error
	if len(pos) > 0 {
		ds, err := gateway.StartDev(ctx, pos, *devPort, os.Stdout)
		if err != nil {
			return err
		}
		defer ds.Stop()
		go func() {
			<-ds.Done()
			if ctx.Err() == nil {
				devExited = fmt.Errorf("the dev server exited (%v)", ds.Err())
				cancel()
			}
		}()
		devURL = ds.URL
	}
	if devURL != nil {
		serving = "the dev server at " + devURL.String()
	}
	key := rand.Text()
	srv := &http.Server{Handler: gateway.Handler(gateway.Config{Controller: base, Token: tok.Secret, Key: key, Web: site, Dev: devURL}), ReadHeaderTimeout: 10 * time.Second}
	link := fmt.Sprintf("http://127.0.0.1:%d/?key=%s", *port, key)
	fmt.Printf("a12s gateway for Fleet %s: %s\n  serving %s and /a12s.js; API as token:%s (%s on %s); Ctrl-C to stop\n",
		f.Metadata.Name, link, serving, tokenName, strings.Join(tok.Actions, ","), strings.Join(tok.Computers, ","))
	if !*noOpen {
		openBrowser(link)
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return devExited
}

const gatewayTokenPrefix = "gateway-"

// staleGatewayTokens are the gateway tokens whose gateway is gone: a gateway holds its port while it runs, so a free
// port (or our own, which we hold now) means its token was left behind by a crash or kill.
func staleGatewayTokens(tokens []api.Token, own int, inUse func(port int) bool) []string {
	var stale []string
	for _, t := range tokens {
		p, err := strconv.Atoi(strings.TrimPrefix(t.Name, gatewayTokenPrefix))
		if !strings.HasPrefix(t.Name, gatewayTokenPrefix) || err != nil || p <= 0 || p > 65535 {
			continue
		}
		if p == own || !inUse(p) {
			stale = append(stale, t.Name)
		}
	}
	return stale
}

func portInUse(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if errors.Is(err, syscall.EADDRINUSE) {
			return true
		}
		if err == nil {
			_ = ln.Close()
		}
	}
	return false
}

// openBrowser is best effort: the link is printed either way.
func openBrowser(link string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	if _, err := exec.LookPath(name); err != nil {
		return
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return
	}
	cmd := exec.Command(name, link)
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
