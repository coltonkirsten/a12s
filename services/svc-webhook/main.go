// Command svc-webhook is the reference webhook bridge:
// it receives HTTP posts on its routes, checks the vendor signature with a
// credential, and enqueues a payload summary with metadata {source, event}.
// It is the only reference Service with a host port (publish:), so it must
// authenticate its callers itself: an unsigned or wrongly signed post is
// rejected and enqueues nothing.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/coltonkirsten/a12s/services/svckit"
)

type route struct {
	Path       string `json:"path"`
	To         string `json:"to"`
	SecretFrom string `json:"secretFrom"`
	Fresh      bool   `json:"fresh,omitempty"`
}

type config struct {
	Routes []route `json:"routes"`
}

// maxPayload bounds a webhook body; the summary is what is enqueued.
const maxPayload = 1 << 20

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stdout)
	var cfg config
	if err := svckit.LoadConfig(&cfg); err != nil {
		log.Fatalf("svc-webhook: config: %v", err)
	}
	client, err := svckit.NewClient()
	if err != nil {
		log.Fatalf("svc-webhook: %v", err)
	}
	mux := http.NewServeMux()
	svckit.Health(mux)
	for _, r := range cfg.Routes {
		if !strings.HasPrefix(r.Path, "/") || r.To == "" || r.SecretFrom == "" {
			log.Fatalf("svc-webhook: route %+v needs path, to and secretFrom", r)
		}
		mux.HandleFunc("POST "+r.Path, handler(client, r))
		log.Printf("svc-webhook: POST %s -> %s (signature: %s)", r.Path, r.To, r.SecretFrom)
	}
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func handler(c *svckit.Client, r route) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, maxPayload+1))
		if err != nil || len(body) > maxPayload {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		secret, err := svckit.Secret(r.SecretFrom)
		if err != nil {
			log.Printf("%s: credential %s: %v", r.Path, r.SecretFrom, err)
			http.Error(w, "not configured", http.StatusServiceUnavailable)
			return
		}
		if !validSignature(secret, body, req.Header.Get("X-Hub-Signature-256")) {
			log.Printf("%s: rejected an unsigned or wrongly signed post from %s", r.Path, req.RemoteAddr)
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		event := req.Header.Get("X-GitHub-Event")
		if event == "" {
			event = "unknown"
		}
		m, code, err := c.Send(r.To, svckit.Enqueue{
			Body:           summarize(event, body),
			Sender:         "webhook:github",
			Metadata:       map[string]string{"source": "github", "event": event},
			Fresh:          r.Fresh,
			IdempotencyKey: delivery(req),
		})
		var ae *svckit.APIError
		switch {
		case errors.As(err, &ae):
			log.Printf("%s: enqueue to %s refused: %v", r.Path, r.To, ae)
			http.Error(w, "refused: "+ae.Message, http.StatusBadGateway)
			return
		case err != nil:
			log.Printf("%s: enqueue to %s failed: %v", r.Path, r.To, err)
			http.Error(w, "enqueue failed", http.StatusBadGateway)
			return
		}
		log.Printf("%s: %s event enqueued to %s: %s (%d)", r.Path, event, r.To, m.ID, code)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": m.ID})
	}
}

// validSignature checks GitHub's X-Hub-Signature-256: sha256=<hex HMAC>.
// An empty secret validates nothing (svckit.Secret already refuses one).
func validSignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	hexsig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexsig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func delivery(req *http.Request) string {
	if d := req.Header.Get("X-GitHub-Delivery"); d != "" && len(d) <= 100 {
		return "github/" + d
	}
	return ""
}

// summarize is the Message body: the event, the repository and the action,
// the title or ref, and the link; then the payload, clipped. It is untrusted
// data like any body.
func summarize(event string, body []byte) string {
	var p struct {
		Action     string `json:"action"`
		Ref        string `json:"ref"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest *struct {
			Title   string `json:"title"`
			HTMLURL string `json:"html_url"`
		} `json:"pull_request"`
		Issue *struct {
			Title   string `json:"title"`
			HTMLURL string `json:"html_url"`
		} `json:"issue"`
	}
	_ = json.Unmarshal(body, &p)
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub %s", event)
	if p.Action != "" {
		fmt.Fprintf(&b, " (%s)", p.Action)
	}
	if p.Repository.FullName != "" {
		fmt.Fprintf(&b, " on %s", p.Repository.FullName)
	}
	if p.Sender.Login != "" {
		fmt.Fprintf(&b, " by %s", p.Sender.Login)
	}
	b.WriteString("\n")
	switch {
	case p.PullRequest != nil:
		fmt.Fprintf(&b, "PR: %s %s\n", p.PullRequest.Title, p.PullRequest.HTMLURL)
	case p.Issue != nil:
		fmt.Fprintf(&b, "Issue: %s %s\n", p.Issue.Title, p.Issue.HTMLURL)
	case p.Ref != "":
		fmt.Fprintf(&b, "Ref: %s\n", p.Ref)
	}
	raw := string(body)
	if len(raw) > 8000 {
		raw = raw[:8000] + "…"
	}
	b.WriteString("\nPayload:\n" + raw)
	return b.String()
}
