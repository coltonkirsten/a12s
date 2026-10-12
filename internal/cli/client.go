package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/coltonkirsten/a12s/internal/api"
)

const (
	controllerFileName   = "controller.json"
	controllerTempPrefix = ".controller.json-"
)

func StateDir(fleetName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return filepath.Join(home, ".a12s", fleetName), nil
}

func ControllerFilePath(fleetName string) (string, error) {
	dir, err := StateDir(fleetName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, controllerFileName), nil
}

// ReadControllerFile wraps fs.ErrNotExist for a missing file.
func ReadControllerFile(fleetName string) (*api.ControllerFile, error) {
	p, err := ControllerFilePath(fleetName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var cf api.ControllerFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &cf, nil
}

// WriteControllerFile keeps the bearer token owner-only (dir 0700, file 0600).
func WriteControllerFile(fleetName string, cf *api.ControllerFile) error {
	p, err := ControllerFilePath(fleetName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("write controller file: %w", err)
	}
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), controllerTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("write controller file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write controller file: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write controller file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write controller file: %w", err)
	}
	return os.Rename(tmp.Name(), p)
}

type Client struct {
	BaseURL string // http://127.0.0.1:<port>
	Token   string
	HTTP    *http.Client
}

func NewClient(cf *api.ControllerFile) *Client {
	return &Client{
		BaseURL: "http://127.0.0.1:" + strconv.Itoa(cf.Port),
		Token:   cf.Token,
		HTTP:    &http.Client{},
	}
}

func Dial(fleetName string) (*Client, error) {
	cf, err := ReadControllerFile(fleetName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no Controller for fleet %s (~/.a12s/%s/controller.json is missing); run a12s up", fleetName, fleetName)
	}
	if err != nil {
		return nil, err
	}
	return NewClient(cf), nil
}

func (c *Client) NewRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// Do returns a non-2xx response as *api.Error.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	req, err := c.NewRequest(ctx, method, path, in)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ResponseError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (c *Client) transportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("the Controller at %s is not reachable (a12s up starts it): %w", c.BaseURL, err)
}

func ResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e api.Error
	if json.Unmarshal(body, &e) == nil && e.Code != "" {
		return &e
	}
	e = api.Error{Code: codeForStatus(resp.StatusCode), Message: string(bytes.TrimSpace(body))}
	if e.Message == "" {
		e.Message = resp.Status
	}
	return &e
}

func codeForStatus(status int) api.ErrorCode {
	switch status {
	case http.StatusConflict:
		return api.ErrBusy
	case http.StatusServiceUnavailable:
		return api.ErrAsleep
	case http.StatusNotFound:
		return api.ErrNotFound
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return api.ErrInvalid
	case http.StatusUnauthorized, http.StatusForbidden:
		return api.ErrUnauthorized
	}
	return api.ErrInternal
}

func HasCode(err error, code api.ErrorCode) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == code
}

func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func NewToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
