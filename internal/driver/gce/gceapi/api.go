// Package gceapi is the Compute Engine and IAM REST subset the GCE driver calls.
package gceapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The Compute Engine and IAM REST calls the driver makes, and nothing else. Field names follow the v1 APIs; int64
// fields travel as JSON strings there.

type Instance struct {
	ID                 uint64              `json:"id,string,omitempty"`
	Name               string              `json:"name"`
	Description        string              `json:"description,omitempty"`
	MachineType        string              `json:"machineType,omitempty"`
	Status             string              `json:"status,omitempty"`
	Labels             map[string]string   `json:"labels,omitempty"`
	LabelFingerprint   string              `json:"labelFingerprint,omitempty"`
	Metadata           *Metadata           `json:"metadata,omitempty"`
	Disks              []AttachedDisk      `json:"disks,omitempty"`
	NetworkInterfaces  []NetworkInterface  `json:"networkInterfaces,omitempty"`
	ServiceAccounts    []ServiceAccountRef `json:"serviceAccounts,omitempty"`
	Scheduling         *Scheduling         `json:"scheduling,omitempty"`
	Tags               *Tags               `json:"tags,omitempty"`
	ShieldedInstance   *ShieldedInstance   `json:"shieldedInstanceConfig,omitempty"`
	LastStartTimestamp string              `json:"lastStartTimestamp,omitempty"`
	SelfLink           string              `json:"selfLink,omitempty"`
}

type Metadata struct {
	Fingerprint string         `json:"fingerprint,omitempty"`
	Items       []MetadataItem `json:"items,omitempty"`
}

type MetadataItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (m *Metadata) Get(key string) (string, bool) {
	if m == nil {
		return "", false
	}
	for _, it := range m.Items {
		if it.Key == key {
			return it.Value, true
		}
	}
	return "", false
}

type AttachedDisk struct {
	DeviceName       string            `json:"deviceName,omitempty"`
	Source           string            `json:"source,omitempty"`
	Boot             bool              `json:"boot,omitempty"`
	AutoDelete       bool              `json:"autoDelete"`
	Mode             string            `json:"mode,omitempty"`
	InitializeParams *InitializeParams `json:"initializeParams,omitempty"`
}

type InitializeParams struct {
	SourceImage string            `json:"sourceImage,omitempty"`
	DiskSizeGb  int64             `json:"diskSizeGb,string,omitempty"`
	DiskType    string            `json:"diskType,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type NetworkInterface struct {
	Network    string `json:"network,omitempty"`
	Subnetwork string `json:"subnetwork,omitempty"`
	NetworkIP  string `json:"networkIP,omitempty"`
}

type ServiceAccountRef struct {
	Email  string   `json:"email"`
	Scopes []string `json:"scopes"`
}

type Scheduling struct {
	AutomaticRestart  *bool  `json:"automaticRestart,omitempty"`
	OnHostMaintenance string `json:"onHostMaintenance,omitempty"`
	ProvisioningModel string `json:"provisioningModel,omitempty"`
}

type Tags struct {
	Items []string `json:"items,omitempty"`
}

type ShieldedInstance struct {
	EnableSecureBoot          bool `json:"enableSecureBoot"`
	EnableVtpm                bool `json:"enableVtpm"`
	EnableIntegrityMonitoring bool `json:"enableIntegrityMonitoring"`
}

type Disk struct {
	ID               uint64            `json:"id,string,omitempty"`
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	SizeGb           int64             `json:"sizeGb,string,omitempty"`
	Type             string            `json:"type,omitempty"`
	Status           string            `json:"status,omitempty"`
	Users            []string          `json:"users,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	LabelFingerprint string            `json:"labelFingerprint,omitempty"`
	ResourcePolicies []string          `json:"resourcePolicies,omitempty"`
	SelfLink         string            `json:"selfLink,omitempty"`
}

type Operation struct {
	ID                  uint64          `json:"id,string,omitempty"`
	Name                string          `json:"name"`
	OperationType       string          `json:"operationType,omitempty"`
	TargetLink          string          `json:"targetLink,omitempty"`
	TargetID            uint64          `json:"targetId,string,omitempty"`
	Status              string          `json:"status,omitempty"`
	StatusMessage       string          `json:"statusMessage,omitempty"`
	User                string          `json:"user,omitempty"`
	InsertTime          string          `json:"insertTime,omitempty"`
	EndTime             string          `json:"endTime,omitempty"`
	HTTPErrorStatusCode int             `json:"httpErrorStatusCode,omitempty"`
	Error               *OperationError `json:"error,omitempty"`
}

type OperationError struct {
	Errors []OperationErrorItem `json:"errors"`
}

type OperationErrorItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Firewall struct {
	Name                  string         `json:"name"`
	Network               string         `json:"network,omitempty"`
	Priority              int            `json:"priority"`
	Direction             string         `json:"direction,omitempty"`
	Disabled              bool           `json:"disabled,omitempty"`
	SourceRanges          []string       `json:"sourceRanges,omitempty"`
	DestinationRanges     []string       `json:"destinationRanges,omitempty"`
	SourceTags            []string       `json:"sourceTags,omitempty"`
	SourceServiceAccounts []string       `json:"sourceServiceAccounts,omitempty"`
	TargetTags            []string       `json:"targetTags,omitempty"`
	TargetServiceAccounts []string       `json:"targetServiceAccounts,omitempty"`
	Allowed               []FirewallRule `json:"allowed,omitempty"`
	Denied                []FirewallRule `json:"denied,omitempty"`
}

type FirewallRule struct {
	IPProtocol string   `json:"IPProtocol"`
	Ports      []string `json:"ports,omitempty"`
}

type Subnetwork struct {
	Name        string `json:"name"`
	Network     string `json:"network,omitempty"`
	IPCidrRange string `json:"ipCidrRange,omitempty"`
	Region      string `json:"region,omitempty"`
	SelfLink    string `json:"selfLink,omitempty"`
}

type MachineType struct {
	Name        string `json:"name"`
	GuestCpus   int64  `json:"guestCpus"`
	MemoryMb    int64  `json:"memoryMb"`
	IsSharedCPU bool   `json:"isSharedCpu,omitempty"`
}

type ServiceAccount struct {
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
	UniqueID    string `json:"uniqueId,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// APIError is a REST error response or a failed operation. Reason is the first error's reason (REST) or code
// (operation), e.g. "notFound", "conditionNotMet", "ZONE_RESOURCE_POOL_EXHAUSTED".
type APIError struct {
	Status  int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("gce: %s: %s", e.Reason, e.Message)
	}
	return fmt.Sprintf("gce: HTTP %d: %s", e.Status, e.Message)
}

func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Reason == "notFound" || e.Reason == "RESOURCE_NOT_FOUND")
}

// IsConflict covers both an existing resource and a stale fingerprint.
func IsConflict(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == http.StatusConflict || e.Status == http.StatusPreconditionFailed ||
		e.Reason == "alreadyExists" || e.Reason == "conditionNotMet" || e.Reason == "RESOURCE_ALREADY_EXISTS")
}

func HasReason(err error, reasons ...string) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	for _, r := range reasons {
		if e.Reason == r {
			return true
		}
	}
	return false
}

// TokenSource returns an OAuth access token for the Controller's service account.
type TokenSource func(ctx context.Context) (string, error)

// API is scoped to one project and zone.
type API struct {
	HTTP    *http.Client
	Compute string // ".../compute/v1/"
	IAM     string // ".../v1/"
	CRM     string // ".../v1/"
	Project string
	Zone    string
	Token   TokenSource
}

const (
	DefaultComputeEndpoint = "https://compute.googleapis.com/compute/v1/"
	DefaultIAMEndpoint     = "https://iam.googleapis.com/v1/"
	DefaultCRMEndpoint     = "https://cloudresourcemanager.googleapis.com/v1/"
)

func (a *API) Region() string {
	if i := strings.LastIndex(a.Zone, "-"); i > 0 {
		return a.Zone[:i]
	}
	return a.Zone
}

func (a *API) zonal(parts ...string) string {
	return a.Compute + "projects/" + a.Project + "/zones/" + a.Zone + "/" + strings.Join(parts, "/")
}

func (a *API) global(parts ...string) string {
	return a.Compute + "projects/" + a.Project + "/global/" + strings.Join(parts, "/")
}

func (a *API) regional(parts ...string) string {
	return a.Compute + "projects/" + a.Project + "/regions/" + a.Region() + "/" + strings.Join(parts, "/")
}

func (a *API) ZoneURL(parts ...string) string { return a.zonal(parts...) }

func (a *API) RegionURL(parts ...string) string { return a.regional(parts...) }

func (a *API) GlobalURL(parts ...string) string { return a.global(parts...) }

func (a *API) do(ctx context.Context, method, u string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.Token != nil {
		tok, err := a.Token(ctx)
		if err != nil {
			return fmt.Errorf("gce: token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	hc := a.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("gce: %s %s: %w", method, redactQuery(u), err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("gce: %s %s: %w", method, redactQuery(u), err)
	}
	if resp.StatusCode >= 300 {
		return parseError(resp.StatusCode, b)
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("gce: %s %s: decode: %w", method, redactQuery(u), err)
	}
	return nil
}

func redactQuery(u string) string {
	base, _, _ := strings.Cut(u, "?")
	return base
}

func parseError(status int, body []byte) error {
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	e := &APIError{Status: status}
	if json.Unmarshal(body, &env) == nil {
		e.Message = env.Error.Message
		if len(env.Error.Errors) > 0 {
			e.Reason = env.Error.Errors[0].Reason
		} else {
			e.Reason = env.Error.Status
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
	}
	return e
}

// list follows nextPageToken; decode receives each page.
func (a *API) list(ctx context.Context, u string, decode func(json.RawMessage) error) error {
	token := ""
	for {
		pu := u
		if token != "" {
			sep := "?"
			if strings.Contains(pu, "?") {
				sep = "&"
			}
			pu += sep + "pageToken=" + url.QueryEscape(token)
		}
		var page struct {
			Items         json.RawMessage `json:"items"`
			NextPageToken string          `json:"nextPageToken"`
		}
		if err := a.do(ctx, http.MethodGet, pu, nil, &page); err != nil {
			return err
		}
		if len(page.Items) > 0 {
			if err := decode(page.Items); err != nil {
				return err
			}
		}
		if page.NextPageToken == "" {
			return nil
		}
		token = page.NextPageToken
	}
}

func withFilter(u, filter string) string {
	if filter == "" {
		return u
	}
	return u + "?filter=" + url.QueryEscape(filter)
}

func listOf[T any](ctx context.Context, a *API, u string) ([]T, error) {
	var out []T
	err := a.list(ctx, u, func(raw json.RawMessage) error {
		var items []T
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		out = append(out, items...)
		return nil
	})
	return out, err
}

func (a *API) GetInstance(ctx context.Context, name string) (*Instance, error) {
	var out Instance
	return &out, a.do(ctx, http.MethodGet, a.zonal("instances", name), nil, &out)
}

func (a *API) ListInstances(ctx context.Context, filter string) ([]Instance, error) {
	return listOf[Instance](ctx, a, withFilter(a.zonal("instances"), filter))
}

func (a *API) InsertInstance(ctx context.Context, in *Instance) error {
	return a.op(ctx, http.MethodPost, a.zonal("instances"), in)
}

func (a *API) StartInstance(ctx context.Context, name string) error {
	return a.op(ctx, http.MethodPost, a.zonal("instances", name, "start"), nil)
}

// StopInstance gives the guest GCE's own shutdown window; the driver stops the container first.
func (a *API) StopInstance(ctx context.Context, name string) error {
	return a.op(ctx, http.MethodPost, a.zonal("instances", name, "stop"), nil)
}

func (a *API) DeleteInstance(ctx context.Context, name string) error {
	return a.op(ctx, http.MethodDelete, a.zonal("instances", name), nil)
}

// SetLabels fails with a conflict when fingerprint is not the instance's current one.
func (a *API) SetLabels(ctx context.Context, name string, labels map[string]string, fingerprint string) error {
	return a.op(ctx, http.MethodPost, a.zonal("instances", name, "setLabels"),
		map[string]any{"labels": labels, "labelFingerprint": fingerprint})
}

func (a *API) SetMetadata(ctx context.Context, name string, md *Metadata) error {
	return a.op(ctx, http.MethodPost, a.zonal("instances", name, "setMetadata"), md)
}

func (a *API) SetMachineType(ctx context.Context, name, machineType string) error {
	return a.op(ctx, http.MethodPost, a.zonal("instances", name, "setMachineType"),
		map[string]string{"machineType": a.zonal("machineTypes", machineType)})
}

func (a *API) AttachDisk(ctx context.Context, name string, d AttachedDisk) error {
	return a.op(ctx, http.MethodPost, a.zonal("instances", name, "attachDisk"), d)
}

func (a *API) DetachDisk(ctx context.Context, name, deviceName string) error {
	return a.op(ctx, http.MethodPost, a.zonal("instances", name, "detachDisk")+"?deviceName="+url.QueryEscape(deviceName), nil)
}

// GuestAttribute reads one value the guest published under namespace/key; only the VM itself can write it.
func (a *API) GuestAttribute(ctx context.Context, name, path string) (string, error) {
	var out struct {
		VariableValue string `json:"variableValue"`
	}
	err := a.do(ctx, http.MethodGet, a.zonal("instances", name, "getGuestAttributes")+"?variableKey="+url.QueryEscape(path), nil, &out)
	return out.VariableValue, err
}

func (a *API) GetDisk(ctx context.Context, name string) (*Disk, error) {
	var out Disk
	return &out, a.do(ctx, http.MethodGet, a.zonal("disks", name), nil, &out)
}

func (a *API) ListDisks(ctx context.Context, filter string) ([]Disk, error) {
	return listOf[Disk](ctx, a, withFilter(a.zonal("disks"), filter))
}

func (a *API) InsertDisk(ctx context.Context, d *Disk) error {
	return a.op(ctx, http.MethodPost, a.zonal("disks"), d)
}

func (a *API) ResizeDisk(ctx context.Context, name string, sizeGb int64) error {
	return a.op(ctx, http.MethodPost, a.zonal("disks", name, "resize"), map[string]string{"sizeGb": fmt.Sprint(sizeGb)})
}

func (a *API) DeleteDisk(ctx context.Context, name string) error {
	return a.op(ctx, http.MethodDelete, a.zonal("disks", name), nil)
}

func (a *API) ListOperations(ctx context.Context, filter string) ([]Operation, error) {
	return listOf[Operation](ctx, a, withFilter(a.zonal("operations"), filter))
}

func (a *API) ListFirewalls(ctx context.Context, filter string) ([]Firewall, error) {
	return listOf[Firewall](ctx, a, withFilter(a.global("firewalls"), filter))
}

func (a *API) GetSubnetwork(ctx context.Context, name string) (*Subnetwork, error) {
	var out Subnetwork
	return &out, a.do(ctx, http.MethodGet, a.regional("subnetworks", name), nil, &out)
}

func (a *API) ListMachineTypes(ctx context.Context, filter string) ([]MachineType, error) {
	return listOf[MachineType](ctx, a, withFilter(a.zonal("machineTypes"), filter))
}

// op starts a zonal operation and waits for it to finish.
func (a *API) op(ctx context.Context, method, u string, in any) error {
	var out Operation
	if err := a.do(ctx, method, u, in, &out); err != nil {
		return err
	}
	return a.Wait(ctx, &out)
}

// Wait blocks until the operation is DONE and returns its error, if any, as an *APIError.
func (a *API) Wait(ctx context.Context, op *Operation) error {
	for op.Status != "DONE" {
		var next Operation
		if err := a.do(ctx, http.MethodPost, a.zonal("operations", op.Name, "wait"), nil, &next); err != nil {
			return err
		}
		op = &next
	}
	if op.Error != nil && len(op.Error.Errors) > 0 {
		first := op.Error.Errors[0]
		msgs := make([]string, 0, len(op.Error.Errors))
		for _, e := range op.Error.Errors {
			msgs = append(msgs, e.Message)
		}
		return &APIError{Status: op.HTTPErrorStatusCode, Reason: first.Code, Message: strings.Join(msgs, "; ")}
	}
	return nil
}

func (a *API) serviceAccountURL(email string) string {
	return a.IAM + "projects/" + a.Project + "/serviceAccounts/" + url.PathEscape(email)
}

func (a *API) CreateServiceAccount(ctx context.Context, accountID, displayName string) (*ServiceAccount, error) {
	var out ServiceAccount
	err := a.do(ctx, http.MethodPost, a.IAM+"projects/"+a.Project+"/serviceAccounts",
		map[string]any{"accountId": accountID, "serviceAccount": map[string]string{"displayName": displayName}}, &out)
	return &out, err
}

func (a *API) GetServiceAccount(ctx context.Context, email string) (*ServiceAccount, error) {
	var out ServiceAccount
	return &out, a.do(ctx, http.MethodGet, a.serviceAccountURL(email), nil, &out)
}

func (a *API) DeleteServiceAccount(ctx context.Context, email string) error {
	return a.do(ctx, http.MethodDelete, a.serviceAccountURL(email), nil, nil)
}

// MissingPermissions returns the ones the caller does not hold on the project.
func (a *API) MissingPermissions(ctx context.Context, perms []string) ([]string, error) {
	var out struct {
		Permissions []string `json:"permissions"`
	}
	if err := a.do(ctx, http.MethodPost, a.CRM+"projects/"+a.Project+":testIamPermissions",
		map[string]any{"permissions": perms}, &out); err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, p := range out.Permissions {
		held[p] = true
	}
	var missing []string
	for _, p := range perms {
		if !held[p] {
			missing = append(missing, p)
		}
	}
	return missing, nil
}

// MetadataToken reads the attached service account's token from the metadata server, cached until a minute before
// it expires.
func MetadataToken(hc *http.Client, base string) TokenSource {
	var mu sync.Mutex
	var tok string
	var exp time.Time
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if tok != "" && time.Now().Before(exp) {
			return tok, nil
		}
		var out struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := metadataGet(ctx, hc, base, "instance/service-accounts/default/token", &out); err != nil {
			return "", err
		}
		tok, exp = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second-time.Minute)
		return tok, nil
	}
}

const DefaultMetadataEndpoint = "http://169.254.169.254/computeMetadata/v1/"

// metadataGet decodes JSON into out when out is not a *string.
func metadataGet(ctx context.Context, hc *http.Client, base, path string, out any) error {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("metadata server: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("metadata server: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metadata server: %s: HTTP %d", path, resp.StatusCode)
	}
	if s, ok := out.(*string); ok {
		*s = strings.TrimSpace(string(b))
		return nil
	}
	return json.Unmarshal(b, out)
}
