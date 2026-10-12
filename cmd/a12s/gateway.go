package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/gateway"
)

const gatewayToken = "gateway"

func runGateway(g *globals, args []string) error {
	flags := g.flags("gateway")
	port := flags.Int("port", 7012, "port on 127.0.0.1")
	web := flags.String("web", "", "directory of your web app to serve (default: the built-in dashboard)")
	dev := flags.String("dev", "", "URL of a running dev server to proxy everything but /v1/ and /a12s.js to")
	devPort := flags.Int("dev-port", 0, "the port of the dev server started after --, when it doesn't print its URL")
	allow := flags.String("allow", "status,events,send,inbox", "API actions the page may use ("+strings.Join(api.TokenActions, ",")+")")
	computers := flags.String("computers", "*", "Computers the page may reach, comma-separated")
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

	// The page gets a scoped token, never the owner's: the Controller enforces --allow and --computers.
	_ = c.Do(ctx, "DELETE", "/v1/tokens/"+gatewayToken, nil, nil)
	var tok api.Token
	if err := c.Do(ctx, "POST", "/v1/tokens", api.TokenRequest{Name: gatewayToken, Actions: list(*allow), Computers: list(*computers)}, &tok); err != nil {
		return fmt.Errorf("create the gateway's token: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Do(ctx, "DELETE", "/v1/tokens/"+gatewayToken, nil, nil)
	}()

	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return fmt.Errorf("%w (pick another with --port)", err)
	}
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
	srv := &http.Server{Handler: gateway.Handler(gateway.Config{Controller: base, Token: tok.Secret, Web: site, Dev: devURL}), ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf("a12s gateway for Fleet %s: http://127.0.0.1:%d\n  serving %s and /a12s.js; API as token:%s (%s on %s); Ctrl-C to stop\n",
		f.Metadata.Name, *port, serving, gatewayToken, strings.Join(tok.Actions, ","), strings.Join(tok.Computers, ","))
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

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
