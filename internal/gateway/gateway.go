// Package gateway lets a browser page reach a Fleet: it serves the page and the browser SDK, and proxies /v1/ to the
// Controller with a scoped token the browser never sees.
package gateway

import (
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

// Handler serves web (or the built-in dashboard when web is nil), the browser SDK at /a12s.js, and the Controller's
// API under /v1/ with token. It answers only requests addressed to a loopback name, so a hostile page can't reach
// it by rebinding its own domain to 127.0.0.1, and it refuses cross-origin writes.
func Handler(controller *url.URL, token string, web fs.FS) http.Handler {
	builtin, _ := fs.Sub(embedded, "web")
	if web == nil {
		web = builtin
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(controller)
			r.Out.Host = controller.Host
			r.Out.Header.Set("Authorization", "Bearer "+token)
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("Origin")
			r.Out.Header.Del("Referer")
		},
		FlushInterval: -1, // event streams
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", proxy)
	mux.Handle("GET /a12s.js", http.FileServerFS(builtin))
	mux.Handle("/", http.FileServerFS(web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			refuse(w, http.StatusForbidden, "the gateway only answers http://127.0.0.1 or http://localhost", "open the page at http://127.0.0.1:<port>")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet && r.Method != http.MethodHead && o != "http://"+r.Host {
			refuse(w, http.StatusForbidden, "cross-origin request from "+o, "serve your page from the gateway (a12s gateway --web <dir>)")
			return
		}
		mux.ServeHTTP(w, r)
	})
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
