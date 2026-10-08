// Package svckit is the little a reference Service needs from a12s: its
// config, its credentials, its mTLS identity toward the Controller, and the
// inbox calls its grants allow. It uses nothing a third-party image doesn't
// get (enhancements §5.8): files under /run/a12s and A12S_API.
package svckit

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Paths a12s gives every Service.
var (
	ConfigFile = "/run/a12s/config/config.json"
	SecretsDir = "/run/a12s/secrets"
	TLSDir     = "/run/a12s/tls"
)

// LoadConfig decodes the Service's config.json into v.
func LoadConfig(v any) error {
	data, err := os.ReadFile(ConfigFile)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Secret reads a credential a12s injected (whitespace trimmed).
func Secret(name string) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || name[0] == '.' {
		return nil, fmt.Errorf("bad credential name %q", name)
	}
	b, err := os.ReadFile(filepath.Join(SecretsDir, name))
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b), nil
}

// Client calls the Controller's mTLS listener as this Service.
type Client struct {
	Base string
	HTTP *http.Client
}

// NewClient builds the client from A12S_API and /run/a12s/tls.
func NewClient() (*Client, error) {
	base := os.Getenv("A12S_API")
	if base == "" {
		return nil, errors.New("A12S_API is not set")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(TLSDir, "cert.pem"), filepath.Join(TLSDir, "key.pem"))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(TLSDir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: u.Hostname(), MinVersion: tls.VersionTLS12},
		Proxy:           nil, // the Controller is on this network; never through the egress proxy
	}
	return &Client{Base: strings.TrimSuffix(base, "/"), HTTP: &http.Client{Transport: tr, Timeout: 20 * time.Second}}, nil
}

// Message is the part of an inbox Message a Service reads back.
type Message struct {
	ID        string     `json:"id"`
	Seq       int64      `json:"seq"`
	State     string     `json:"state"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Replaced  string     `json:"replaced,omitempty"`
}

// Enqueue is one send.
type Enqueue struct {
	Body           string            `json:"body"`
	Sender         string            `json:"sender,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Fresh          bool              `json:"fresh,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
	Expires        *int64            `json:"expires,omitempty"`
	ReplaceKey     string            `json:"replaceKey,omitempty"`
}

// APIError is a refused call: the HTTP status and the Controller's error.
type APIError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
	if e.Fix != "" {
		s += "  fix: " + e.Fix
	}
	return s
}

func (c *Client) do(method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		e := &APIError{Status: res.StatusCode}
		_ = json.Unmarshal(data, e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return res.StatusCode, e
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return res.StatusCode, err
		}
	}
	return res.StatusCode, nil
}

// Send enqueues to a Computer the Service's grants name.
func (c *Client) Send(computer string, m Enqueue) (Message, int, error) {
	var out Message
	code, err := c.do(http.MethodPost, "/v1/computers/"+url.PathEscape(computer)+"/inbox", m, &out)
	return out, code, err
}

// Get reads back one of the Service's own Messages.
func (c *Client) Get(computer, id string) (Message, error) {
	var out Message
	_, err := c.do(http.MethodGet, "/v1/computers/"+url.PathEscape(computer)+"/inbox/"+url.PathEscape(id), nil, &out)
	return out, err
}

// ParseDuration reads 0, 90s, 15m, 1h or 7d.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if d, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(d, "%d", &n); err != nil {
			return 0, fmt.Errorf("%q is not a duration", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// Health serves GET /healthz on addr with ok, in the background.
func Health(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
}
