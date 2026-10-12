// Package gateway lets a browser page reach a Fleet: it serves the page (or proxies a dev server) and the browser
// SDK, and proxies /v1/ to the Controller with a scoped token the browser never sees.
package gateway

import (
	"crypto/subtle"
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
	// Key unlocks the gateway: in ?key= once (it becomes a cookie) or as a bearer token. Required.
	Key string
	// Web is the app to serve; nil serves the built-in dashboard. Ignored when Dev is set.
	Web fs.FS
	// Dev is a running dev server that gets every request except /v1/ and /a12s.js, websocket upgrades included.
	Dev *url.URL
}

// Handler serves the app, the browser SDK at /a12s.js, and the Controller's API under /v1/ with cfg.Token. It answers
// only requests addressed to a loopback name, so a hostile page can't reach it by rebinding its own domain to
// 127.0.0.1, and it refuses cross-origin writes. /v1/ and /a12s.js need cfg.Key, so another local user or
// program can't use the Fleet through it either.
func Handler(cfg Config) http.Handler {
	if cfg.Key == "" {
		panic("gateway: Config.Key is required")
	}
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
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !LoopbackHost(host) {
			refuse(w, http.StatusForbidden, "the gateway only answers http://127.0.0.1 or http://localhost on this machine, not "+r.Host, ServeOthers)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet && r.Method != http.MethodHead && o != "http://"+r.Host {
			refuse(w, http.StatusForbidden, "cross-origin request from "+o, "serve your page from the gateway (a12s gateway --web <dir>, or a12s gateway -- npm run dev)")
			return
		}
		if k, ok := r.URL.Query()["key"]; ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if !sameKey(k[0], cfg.Key) {
				refuse(w, http.StatusForbidden, "wrong gateway key", "open the link a12s gateway printed when it started")
				return
			}
			unlock(w, r, cfg.Key)
			return
		}
		if !hasKey(r, cfg.Key) {
			if r.URL.Path == "/a12s.js" || strings.HasPrefix(r.URL.Path, "/v1/") {
				refuse(w, http.StatusUnauthorized, "this gateway needs its key", "open the link a12s gateway printed (http://127.0.0.1:<port>/?key=…), or send Authorization: Bearer <key>")
				return
			}
			if r.Header.Get("Sec-Fetch-Mode") == "navigate" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(lockedPage))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

const lockedPage = `<!doctype html><meta charset="utf-8"><title>a12s gateway</title>
<p style="font:16px system-ui;margin:3em">This a12s gateway is locked. Open the link it printed in your terminal
(<code>http://127.0.0.1:&lt;port&gt;/?key=…</code>).</p>`

// cookieName carries the port because cookies are shared by every port on a host: two gateways keep two keys.
func cookieName(r *http.Request) string {
	_, port, _ := net.SplitHostPort(r.Host)
	return "a12s-gateway-" + port
}

func sameKey(got, key string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

func hasKey(r *http.Request, key string) bool {
	if c, err := r.Cookie(cookieName(r)); err == nil && sameKey(c.Value, key) {
		return true
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && sameKey(bearer, key)
}

// unlock trades ?key= for a cookie and redirects to the same URL without the key, so it leaves the address bar and
// never reaches the app or its Referer headers.
func unlock(w http.ResponseWriter, r *http.Request, key string) {
	http.SetCookie(w, &http.Cookie{Name: cookieName(r), Value: key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	q := r.URL.Query()
	q.Del("key")
	to := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, to.String(), http.StatusSeeOther)
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

// ServeOthers is what to do instead of exposing the gateway, which holds a Fleet token behind nothing but a key.
const ServeOthers = "To serve other people, put your own server with login in front of a scoped token; see docs/sdk.md#serve-other-people"

// LoopbackHost reports whether host (no port) names this machine only.
func LoopbackHost(host string) bool {
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
