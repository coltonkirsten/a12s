// Package credsource reads credential values from the external stores a Fleet's credentials name.
package credsource

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// ErrNoValue means the source answered and has no usable value: the secret, or its latest version, is gone,
// disabled or destroyed. The Controller drops the value it held. Any other error is treated as passing.
var ErrNoValue = errors.New("the source has no usable value")

// Secret is a value read from a source and the source's name for the version it came from.
type Secret struct {
	Value   []byte
	Version string
}

type Resolver interface {
	Resolve(ctx context.Context, src fleet.CredentialSource) (Secret, error)
}

const DefaultSecretManagerEndpoint = "https://secretmanager.googleapis.com/v1/"

// GCP reads Secret Manager with the service account of the VM the Controller runs on, from its metadata server: no
// key is stored anywhere.
type GCP struct {
	HTTP     *http.Client
	Endpoint string
	Token    gceapi.TokenSource
}

func NewGCP() *GCP {
	hc := &http.Client{}
	return &GCP{HTTP: hc, Endpoint: DefaultSecretManagerEndpoint, Token: gceapi.MetadataToken(hc, gceapi.DefaultMetadataEndpoint)}
}

func (g *GCP) Resolve(ctx context.Context, src fleet.CredentialSource) (Secret, error) {
	if src.GCPSecret == "" {
		return Secret{}, fmt.Errorf("no source set")
	}
	token, err := g.Token(ctx)
	if err != nil {
		return Secret{}, fmt.Errorf("%s: the Controller's service account token: %w (gcpSecret needs the Controller on a GCE VM)", src.GCPSecret, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Endpoint+src.GCPSecret+"/versions/latest:access", nil)
	if err != nil {
		return Secret{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return Secret{}, fmt.Errorf("%s: %w", src.GCPSecret, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Secret{}, fmt.Errorf("%s: %w", src.GCPSecret, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Secret{}, accessError(src.GCPSecret, resp.StatusCode, body)
	}
	var out struct {
		Name    string `json:"name"`
		Payload struct {
			Data       string          `json:"data"`
			DataCrc32c json.RawMessage `json:"dataCrc32c"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Secret{}, fmt.Errorf("%s: unreadable answer from Secret Manager", src.GCPSecret)
	}
	data, err := base64.StdEncoding.DecodeString(out.Payload.Data)
	if err != nil {
		return Secret{}, fmt.Errorf("%s: unreadable payload from Secret Manager", src.GCPSecret)
	}
	// An int64 in Google's JSON: usually a quoted string, but a number is accepted too.
	if c := strings.Trim(string(out.Payload.DataCrc32c), `"`); c != "" && c != strconv.FormatUint(uint64(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))), 10) {
		return Secret{}, fmt.Errorf("%s: the payload's checksum does not match", src.GCPSecret)
	}
	value := bytes.TrimSpace(data)
	if len(value) == 0 {
		return Secret{}, fmt.Errorf("%s: %s is empty: %w", src.GCPSecret, out.Name, ErrNoValue)
	}
	return Secret{Value: value, Version: out.Name}, nil
}

// accessError keeps only Google's status and message: the error body never holds the payload.
func accessError(secret string, status int, body []byte) error {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := strings.TrimSpace(e.Error.Message)
	if msg == "" {
		msg = http.StatusText(status)
	}
	err := fmt.Errorf("%s: Secret Manager answered %d %s: %s", secret, status, e.Error.Status, msg)
	// latest is the newest version whatever its state: disabling or destroying it, or deleting the secret, is how
	// an owner withdraws the value.
	if status == http.StatusNotFound || e.Error.Status == "NOT_FOUND" || e.Error.Status == "FAILED_PRECONDITION" {
		return fmt.Errorf("%w: %w", err, ErrNoValue)
	}
	return err
}
