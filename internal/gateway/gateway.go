// Package gateway lets a browser page reach a Fleet: it serves the page (or proxies a dev server) and the browser
// SDK, and proxies /v1/ to the Controller with a scoped token the browser never sees.
package gateway

import (
	"crypto/tls"
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
)

//go:embed web
var embedded embed.FS

type Config struct {
	Controller *url.URL
	Token      string
	// Web is the app to serve; nil serves the built-in dashboard. Ignored when Dev is set.
	Web fs.FS
	// Dev is a running dev server that gets every request except /v1/ and /a12s.js, websocket upgrades included.
	Dev *url.URL
}

// Handler serves the app, the browser SDK at /a12s.js, and the Controller's API under /v1/ with cfg.Token. It answers
// only requests addressed to a loopback name, so a hostile page can't reach it by rebinding its own domain to
// 127.0.0.1, and it refuses cross-origin writes.
func Handler(cfg Config) http.Handler {
	builtin, _ := fs.Sub(embedded, "web")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(cfg.Controller)
			r.Out.Host = cfg.Controller.Host
			r.Out.Header.Set("Authorization", "Bearer "+cfg.Token)
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("Origin")
			r.Out.Header.Del("Referer")
		},
		FlushInterval: -1, // event streams
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", proxy)
	mux.Handle("GET /a12s.js", http.FileServerFS(builtin))
	switch {
	case cfg.Dev != nil:
		mux.Handle("/", devProxy(cfg.Dev))
	case cfg.Web != nil:
		mux.Handle("/", http.FileServerFS(cfg.Web))
	default:
		mux.Handle("/", http.FileServerFS(builtin))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			refuse(w, http.StatusForbidden, "the gateway only answers http://127.0.0.1 or http://localhost", "open the page at http://127.0.0.1:<port>")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet && r.Method != http.MethodHead && o != "http://"+r.Host {
			refuse(w, http.StatusForbidden, "cross-origin request from "+o, "serve your page from the gateway (a12s gateway --web <dir>, or a12s gateway -- npm run dev)")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// devProxy keeps the browser's Host so the dev server builds its URLs (and its HMR websocket) against the gateway's
// origin, not its own port.
func devProxy(dev *url.URL) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(dev)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		Transport:     devTransport(),
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("a12s gateway: the dev server at " + dev.String() + " is not answering (" + err.Error() + ")\n"))
		},
	}
}

func devTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// A dev server's self-signed certificate is the norm, and it is on this machine.
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return t
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func refuse(w http.ResponseWriter, status int, msg, fix string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Error{Code: api.ErrForbidden, Message: msg, Fix: fix})
}
