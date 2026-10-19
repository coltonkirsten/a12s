// Package gcefake is an in-memory Compute Engine and IAM API for tests: the REST subset the GCE driver calls, with
// fingerprints, state transitions, and injected failures.
package gcefake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

type Fake struct {
	Server *httptest.Server

	mu         sync.Mutex
	project    string
	zone       string
	nextID     uint64
	nextIP     int
	clock      time.Time
	instances  map[string]*gceapi.Instance
	disks      map[string]*gceapi.Disk
	ops        []gceapi.Operation
	firewalls  []gceapi.Firewall
	accounts   map[string]*gceapi.ServiceAccount
	guest      map[string]map[string]string
	denied     map[string]bool
	httpFaults []httpFault
	opFaults   []opFault
	calls      []string
	types      []gceapi.MachineType
}

type httpFault struct {
	call   string // "instances.insert"
	target string // resource name, "" for any
	status int
	reason string
}

type opFault struct {
	opType string
	target string
	code   string
}

// Standard machine types of the families the driver supports, as a zone lists them.
var MachineTypes = []gceapi.MachineType{
	{Name: "e2-micro", GuestCpus: 2, MemoryMb: 1024, IsSharedCPU: true},
	{Name: "e2-small", GuestCpus: 2, MemoryMb: 2048, IsSharedCPU: true},
	{Name: "e2-medium", GuestCpus: 2, MemoryMb: 4096, IsSharedCPU: true},
	{Name: "e2-standard-2", GuestCpus: 2, MemoryMb: 8192},
	{Name: "e2-standard-4", GuestCpus: 4, MemoryMb: 16384},
	{Name: "e2-highmem-2", GuestCpus: 2, MemoryMb: 16384},
	{Name: "e2-highcpu-2", GuestCpus: 2, MemoryMb: 2048},
	{Name: "n2d-standard-2", GuestCpus: 2, MemoryMb: 8192},
	{Name: "n2d-standard-4", GuestCpus: 4, MemoryMb: 16384},
	{Name: "t2d-standard-1", GuestCpus: 1, MemoryMb: 4096},
	{Name: "t2d-standard-2", GuestCpus: 2, MemoryMb: 8192},
	{Name: "t2a-standard-1", GuestCpus: 1, MemoryMb: 4096},
	{Name: "t2a-standard-2", GuestCpus: 2, MemoryMb: 8192},
}

func New(project, zone string) *Fake {
	f := &Fake{
		project:   project,
		zone:      zone,
		nextID:    1000,
		nextIP:    10,
		clock:     time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
		instances: map[string]*gceapi.Instance{},
		disks:     map[string]*gceapi.Disk{},
		accounts:  map[string]*gceapi.ServiceAccount{},
		guest:     map[string]map[string]string{},
		denied:    map[string]bool{},
		types:     slices.Clone(MachineTypes),
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) Close() { f.Server.Close() }

// API returns a client of this fake.
func (f *Fake) API() *gceapi.API {
	return &gceapi.API{
		HTTP:    f.Server.Client(),
		Compute: f.Server.URL + "/compute/v1/",
		IAM:     f.Server.URL + "/iam/v1/",
		CRM:     f.Server.URL + "/crm/v1/",
		Project: f.project,
		Zone:    f.zone,
	}
}

// FailHTTP makes the next matching call fail with status and reason; target "" matches any resource.
func (f *Fake) FailHTTP(call, target string, status int, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.httpFaults = append(f.httpFaults, httpFault{call, target, status, reason})
}

// FailOp makes the next matching operation finish with error code, changing nothing.
func (f *Fake) FailOp(opType, target, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opFaults = append(f.opFaults, opFault{opType, target, code})
}

// Deny makes testIamPermissions leave out perm.
func (f *Fake) Deny(perm string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied[perm] = true
}

func (f *Fake) SetFirewalls(fw []gceapi.Firewall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firewalls = slices.Clone(fw)
}

func (f *Fake) SetGuestAttribute(instance, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.guest[instance] == nil {
		f.guest[instance] = map[string]string{}
	}
	f.guest[instance][key] = value
}

// HostError terminates an instance the way a host failure does, with the system operation recording it.
func (f *Fake) HostError(name string) {
	f.systemEvent(name, "compute.instances.hostError", "TERMINATED")
}

// GuestTerminate is a shutdown from inside the VM.
func (f *Fake) GuestTerminate(name string) {
	f.systemEvent(name, "compute.instances.guestTerminate", "TERMINATED")
}

func (f *Fake) systemEvent(name, opType, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.instances[name]
	if in == nil {
		return
	}
	in.Status = status
	f.recordOp(opType, in.SelfLink, in.ID, "system", nil)
}

// SetStatus moves an instance to a state without an operation (PROVISIONING, STAGING, STOPPING...).
func (f *Fake) SetStatus(name, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instances[name]; in != nil {
		in.Status = status
	}
}

func (f *Fake) Instance(name string) (gceapi.Instance, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.instances[name]
	if !ok {
		return gceapi.Instance{}, false
	}
	return clone(*in), true
}

func (f *Fake) Disk(name string) (gceapi.Disk, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.disks[name]
	if !ok {
		return gceapi.Disk{}, false
	}
	return clone(*d), true
}

func (f *Fake) ServiceAccounts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for e := range f.accounts {
		out = append(out, e)
	}
	slices.Sort(out)
	return out
}

// Calls returns and clears the mutating calls made so far, as "instances.start a12s-lab-scout".
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func clone[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *Fake) tick() string {
	f.clock = f.clock.Add(time.Second)
	return f.clock.Format(time.RFC3339Nano)
}

func (f *Fake) fingerprint() string {
	f.nextID++
	return "fp" + strconv.FormatUint(f.nextID, 36)
}

func (f *Fake) zonalLink(kind, name string) string {
	return f.Server.URL + "/compute/v1/projects/" + f.project + "/zones/" + f.zone + "/" + kind + "/" + name
}

func (f *Fake) recordOp(opType, target string, targetID uint64, user string, opErr *gceapi.OperationError) gceapi.Operation {
	f.nextID++
	t := f.tick()
	op := gceapi.Operation{
		ID: f.nextID, Name: "operation-" + strconv.FormatUint(f.nextID, 10), OperationType: opType,
		TargetLink: target, TargetID: targetID, Status: "DONE", User: user, InsertTime: t, EndTime: t, Error: opErr,
	}
	if opErr != nil {
		op.HTTPErrorStatusCode = http.StatusServiceUnavailable
	}
	f.ops = append(f.ops, op)
	return op
}

var (
	zonalRE  = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/([a-zA-Z]+)(?:/([^/]+))?(?:/([a-zA-Z]+))?$`)
	globalRE = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/global/([a-zA-Z]+)$`)
	saRE     = regexp.MustCompile(`^/iam/v1/projects/([^/]+)/serviceAccounts(?:/([^/]+))?$`)
	crmRE    = regexp.MustCompile(`^/crm/v1/projects/([^/:]+):testIamPermissions$`)
)

type reply struct {
	status int
	body   any
}

func errReply(status int, reason, format string, args ...any) reply {
	msg := fmt.Sprintf(format, args...)
	return reply{status, map[string]any{"error": map[string]any{
		"code": status, "message": msg, "errors": []map[string]string{{"reason": reason, "message": msg}},
	}}}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	rep := f.route(r, body)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	if rep.body != nil {
		_ = json.NewEncoder(w).Encode(rep.body)
	}
}

func (f *Fake) route(r *http.Request, body []byte) reply {
	p := r.URL.Path
	if m := zonalRE.FindStringSubmatch(p); m != nil {
		if m[1] != f.project || m[2] != f.zone {
			return errReply(404, "notFound", "unknown project or zone %s/%s", m[1], m[2])
		}
		return f.zonal(r, m[3], m[4], m[5], body)
	}
	if m := globalRE.FindStringSubmatch(p); m != nil && m[2] == "firewalls" && r.Method == http.MethodGet {
		return f.listReply(f.firewalls, r)
	}
	if m := saRE.FindStringSubmatch(p); m != nil {
		return f.serviceAccounts(r, m[2], body)
	}
	if m := crmRE.FindStringSubmatch(p); m != nil && r.Method == http.MethodPost {
		var req struct {
			Permissions []string `json:"permissions"`
		}
		_ = json.Unmarshal(body, &req)
		var held []string
		for _, perm := range req.Permissions {
			if !f.denied[perm] {
				held = append(held, perm)
			}
		}
		return reply{200, map[string]any{"permissions": held}}
	}
	return errReply(404, "notFound", "no route for %s %s", r.Method, p)
}

func (f *Fake) fault(call, target string) (reply, bool) {
	for i, h := range f.httpFaults {
		if h.call == call && (h.target == "" || h.target == target) {
			f.httpFaults = slices.Delete(f.httpFaults, i, i+1)
			return errReply(h.status, h.reason, "injected %s failure", call), true
		}
	}
	return reply{}, false
}

func (f *Fake) opFault(opType, target string) *gceapi.OperationError {
	for i, o := range f.opFaults {
		if o.opType == opType && (o.target == "" || o.target == target) {
			f.opFaults = slices.Delete(f.opFaults, i, i+1)
			return &gceapi.OperationError{Errors: []gceapi.OperationErrorItem{{Code: o.code, Message: "injected " + o.code}}}
		}
	}
	return nil
}

func (f *Fake) zonal(r *http.Request, kind, name, verb string, body []byte) reply {
	call := kind + "." + strings.ToLower(r.Method)
	switch {
	case verb != "":
		call = kind + "." + verb
	case r.Method == http.MethodPost && name == "":
		call = kind + ".insert"
	case r.Method == http.MethodGet && name == "":
		call = kind + ".list"
	case r.Method == http.MethodGet:
		call = kind + ".get"
	}
	target := name
	if call == "instances.insert" || call == "disks.insert" {
		var n struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &n)
		target = n.Name
	}
	if rep, ok := f.fault(call, target); ok {
		return rep
	}
	if r.Method != http.MethodGet && call != "operations.wait" {
		f.calls = append(f.calls, strings.TrimSpace(call+" "+target))
	}
	switch kind {
	case "instances":
		return f.instance(r, call, name, body)
	case "disks":
		return f.disk(r, call, name, body)
	case "operations":
		switch call {
		case "operations.list":
			return f.listReply(f.ops, r)
		case "operations.wait", "operations.get":
			for _, op := range f.ops {
				if op.Name == name {
					return reply{200, op}
				}
			}
			return errReply(404, "notFound", "operation %s", name)
		}
	case "machineTypes":
		if call == "machineTypes.list" {
			return f.listReply(f.types, r)
		}
	}
	return errReply(404, "notFound", "no route for %s", call)
}

func (f *Fake) done(opType string, target string, id uint64) reply {
	return reply{200, f.recordOp(opType, target, id, "a12s-controller", nil)}
}

func (f *Fake) failedOp(opType, target string, id uint64, e *gceapi.OperationError) reply {
	return reply{200, f.recordOp(opType, target, id, "a12s-controller", e)}
}

func (f *Fake) instance(r *http.Request, call, name string, body []byte) reply {
	switch call {
	case "instances.list":
		var list []gceapi.Instance
		for _, in := range f.instances {
			list = append(list, *in)
		}
		slices.SortFunc(list, func(a, b gceapi.Instance) int { return strings.Compare(a.Name, b.Name) })
		return f.listReply(list, r)
	case "instances.insert":
		var in gceapi.Instance
		if err := json.Unmarshal(body, &in); err != nil {
			return errReply(400, "invalid", "bad instance: %v", err)
		}
		if _, ok := f.instances[in.Name]; ok {
			return errReply(409, "alreadyExists", "instance %s already exists", in.Name)
		}
		link := f.zonalLink("instances", in.Name)
		f.nextID++
		in.ID, in.SelfLink = f.nextID, link
		if e := f.opFault("insert", in.Name); e != nil {
			return f.failedOp("insert", link, in.ID, e)
		}
		for _, sa := range in.ServiceAccounts {
			if f.accounts[sa.Email] == nil {
				return errReply(400, "invalid", "The resource 'projects/%s/serviceAccounts/%s' was not found", f.project, sa.Email)
			}
		}
		for i, d := range in.Disks {
			if d.Boot && d.InitializeParams != nil {
				bootName := in.Name
				f.nextID++
				f.disks[bootName] = &gceapi.Disk{
					ID: f.nextID, Name: bootName, SizeGb: max(d.InitializeParams.DiskSizeGb, 10), Type: d.InitializeParams.DiskType,
					Status: "READY", Users: []string{link}, SelfLink: f.zonalLink("disks", bootName), LabelFingerprint: f.fingerprint(),
				}
				in.Disks[i].Source = f.disks[bootName].SelfLink
				continue
			}
			disk := f.disks[last(d.Source)]
			if disk == nil {
				return errReply(400, "invalid", "disk %s not found", d.Source)
			}
			if len(disk.Users) > 0 {
				return errReply(400, "resourceInUseByAnotherResource", "disk %s is in use by %s", disk.Name, disk.Users[0])
			}
			disk.Users = []string{link}
		}
		for i := range in.NetworkInterfaces {
			f.nextIP++
			in.NetworkInterfaces[i].NetworkIP = "10.128.0." + strconv.Itoa(f.nextIP)
		}
		in.Status = "RUNNING"
		in.LastStartTimestamp = f.tick()
		in.LabelFingerprint = f.fingerprint()
		if in.Metadata == nil {
			in.Metadata = &gceapi.Metadata{}
		}
		in.Metadata.Fingerprint = f.fingerprint()
		f.instances[in.Name] = &in
		return f.done("insert", link, in.ID)
	}
	in := f.instances[name]
	if in == nil {
		return errReply(404, "notFound", "The resource 'projects/%s/zones/%s/instances/%s' was not found", f.project, f.zone, name)
	}
	switch call {
	case "instances.get":
		return reply{200, in}
	case "instances.getGuestAttributes":
		key := r.URL.Query().Get("variableKey")
		v, ok := f.guest[name][key]
		if !ok {
			return errReply(404, "notFound", "guest attribute %s not found", key)
		}
		return reply{200, map[string]string{"variableKey": key, "variableValue": v}}
	case "instances.start":
		if e := f.opFault("start", name); e != nil {
			return f.failedOp("start", in.SelfLink, in.ID, e)
		}
		if in.Status != "RUNNING" {
			in.Status = "RUNNING"
			in.LastStartTimestamp = f.tick()
			f.nextIP++
			for i := range in.NetworkInterfaces {
				in.NetworkInterfaces[i].NetworkIP = "10.128.0." + strconv.Itoa(f.nextIP)
			}
		}
		return f.done("start", in.SelfLink, in.ID)
	case "instances.stop":
		if e := f.opFault("stop", name); e != nil {
			return f.failedOp("stop", in.SelfLink, in.ID, e)
		}
		in.Status = "TERMINATED"
		return f.done("stop", in.SelfLink, in.ID)
	case "instances.delete":
		if e := f.opFault("delete", name); e != nil {
			return f.failedOp("delete", in.SelfLink, in.ID, e)
		}
		for _, d := range in.Disks {
			disk := f.disks[last(d.Source)]
			if disk == nil {
				continue
			}
			if d.AutoDelete {
				delete(f.disks, disk.Name)
			} else {
				disk.Users = slices.DeleteFunc(disk.Users, func(u string) bool { return u == in.SelfLink })
			}
		}
		delete(f.instances, name)
		delete(f.guest, name)
		return f.done("delete", in.SelfLink, in.ID)
	case "instances.setLabels":
		var req struct {
			Labels           map[string]string `json:"labels"`
			LabelFingerprint string            `json:"labelFingerprint"`
		}
		_ = json.Unmarshal(body, &req)
		if req.LabelFingerprint != in.LabelFingerprint {
			return errReply(412, "conditionNotMet", "Labels fingerprint either invalid or resource labels have changed")
		}
		in.Labels = req.Labels
		in.LabelFingerprint = f.fingerprint()
		return f.done("setLabels", in.SelfLink, in.ID)
	case "instances.setMetadata":
		var md gceapi.Metadata
		_ = json.Unmarshal(body, &md)
		if in.Metadata == nil || md.Fingerprint != in.Metadata.Fingerprint {
			return errReply(412, "conditionNotMet", "Supplied fingerprint does not match current metadata fingerprint")
		}
		md.Fingerprint = f.fingerprint()
		in.Metadata = &md
		return f.done("setMetadata", in.SelfLink, in.ID)
	case "instances.setMachineType":
		var req struct {
			MachineType string `json:"machineType"`
		}
		_ = json.Unmarshal(body, &req)
		if in.Status != "TERMINATED" {
			return errReply(400, "resourceNotReady", "instance %s must be stopped to change its machine type", name)
		}
		in.MachineType = req.MachineType
		return f.done("setMachineType", in.SelfLink, in.ID)
	case "instances.attachDisk":
		var d gceapi.AttachedDisk
		_ = json.Unmarshal(body, &d)
		disk := f.disks[last(d.Source)]
		if disk == nil {
			return errReply(400, "invalid", "disk %s not found", d.Source)
		}
		if len(disk.Users) > 0 {
			return errReply(400, "resourceInUseByAnotherResource", "disk %s is in use by %s", disk.Name, disk.Users[0])
		}
		disk.Users = []string{in.SelfLink}
		in.Disks = append(in.Disks, d)
		return f.done("attachDisk", in.SelfLink, in.ID)
	case "instances.detachDisk":
		dev := r.URL.Query().Get("deviceName")
		for i, d := range in.Disks {
			if d.DeviceName == dev {
				if disk := f.disks[last(d.Source)]; disk != nil {
					disk.Users = slices.DeleteFunc(disk.Users, func(u string) bool { return u == in.SelfLink })
				}
				in.Disks = slices.Delete(in.Disks, i, i+1)
				return f.done("detachDisk", in.SelfLink, in.ID)
			}
		}
		return errReply(400, "invalid", "no disk with device name %s", dev)
	}
	return errReply(404, "notFound", "no route for %s", call)
}

func (f *Fake) disk(r *http.Request, call, name string, body []byte) reply {
	switch call {
	case "disks.list":
		var list []gceapi.Disk
		for _, d := range f.disks {
			list = append(list, *d)
		}
		slices.SortFunc(list, func(a, b gceapi.Disk) int { return strings.Compare(a.Name, b.Name) })
		return f.listReply(list, r)
	case "disks.insert":
		var d gceapi.Disk
		if err := json.Unmarshal(body, &d); err != nil {
			return errReply(400, "invalid", "bad disk: %v", err)
		}
		if _, ok := f.disks[d.Name]; ok {
			return errReply(409, "alreadyExists", "disk %s already exists", d.Name)
		}
		f.nextID++
		d.ID, d.SelfLink, d.Status = f.nextID, f.zonalLink("disks", d.Name), "READY"
		if d.SizeGb == 0 {
			d.SizeGb = 10
		}
		d.LabelFingerprint = f.fingerprint()
		if e := f.opFault("insert", d.Name); e != nil {
			return f.failedOp("insert", d.SelfLink, d.ID, e)
		}
		f.disks[d.Name] = &d
		return f.done("insert", d.SelfLink, d.ID)
	}
	d := f.disks[name]
	if d == nil {
		return errReply(404, "notFound", "The resource 'projects/%s/zones/%s/disks/%s' was not found", f.project, f.zone, name)
	}
	switch call {
	case "disks.get":
		return reply{200, d}
	case "disks.resize":
		var req struct {
			SizeGb int64 `json:"sizeGb,string"`
		}
		_ = json.Unmarshal(body, &req)
		if req.SizeGb <= d.SizeGb {
			return errReply(400, "invalid", "new size %d must be larger than %d", req.SizeGb, d.SizeGb)
		}
		d.SizeGb = req.SizeGb
		return f.done("resize", d.SelfLink, d.ID)
	case "disks.delete":
		if len(d.Users) > 0 {
			return errReply(400, "resourceInUseByAnotherResource", "The disk resource '%s' is already being used by '%s'", d.Name, d.Users[0])
		}
		delete(f.disks, name)
		return f.done("delete", d.SelfLink, d.ID)
	}
	return errReply(404, "notFound", "no route for %s", call)
}

func (f *Fake) serviceAccounts(r *http.Request, email string, body []byte) reply {
	switch {
	case email == "" && r.Method == http.MethodPost:
		var req struct {
			AccountID      string `json:"accountId"`
			ServiceAccount struct {
				DisplayName string `json:"displayName"`
			} `json:"serviceAccount"`
		}
		_ = json.Unmarshal(body, &req)
		if rep, ok := f.fault("serviceAccounts.create", req.AccountID); ok {
			return rep
		}
		f.calls = append(f.calls, "serviceAccounts.create "+req.AccountID)
		addr := req.AccountID + "@" + f.project + ".iam.gserviceaccount.com"
		if f.accounts[addr] != nil {
			return errReply(409, "alreadyExists", "Service account %s already exists within project", req.AccountID)
		}
		f.nextID++
		sa := &gceapi.ServiceAccount{
			Name: "projects/" + f.project + "/serviceAccounts/" + addr, Email: addr,
			UniqueID: strconv.FormatUint(f.nextID, 10), DisplayName: req.ServiceAccount.DisplayName,
		}
		f.accounts[addr] = sa
		return reply{200, sa}
	case email != "" && r.Method == http.MethodGet:
		if sa := f.accounts[email]; sa != nil {
			return reply{200, sa}
		}
		return errReply(404, "notFound", "Unknown service account %s", email)
	case email != "" && r.Method == http.MethodDelete:
		if rep, ok := f.fault("serviceAccounts.delete", email); ok {
			return rep
		}
		f.calls = append(f.calls, "serviceAccounts.delete "+email)
		if f.accounts[email] == nil {
			return errReply(404, "notFound", "Unknown service account %s", email)
		}
		delete(f.accounts, email)
		return reply{200, map[string]any{}}
	}
	return errReply(404, "notFound", "no route for %s %s", r.Method, r.URL.Path)
}

func last(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

// listReply applies the filter subset the driver uses: space-separated terms, all of which must hold, each
// `labels.K=V`, `name=V`, `operationType=V` or `insertTime>"T"`, optionally in parentheses.
func (f *Fake) listReply(items any, r *http.Request) reply {
	filter := r.URL.Query().Get("filter")
	raw, _ := json.Marshal(items)
	var objs []map[string]any
	_ = json.Unmarshal(raw, &objs)
	var out []map[string]any
	for _, o := range objs {
		if matches(o, filter) {
			out = append(out, o)
		}
	}
	if out == nil {
		return reply{200, map[string]any{}}
	}
	return reply{200, map[string]any{"items": out}}
}

var termRE = regexp.MustCompile(`\(?\s*([a-zA-Z0-9_.-]+)\s*(=|>|<|!=)\s*"?([^"()\s]*)"?\s*\)?`)

func matches(o map[string]any, filter string) bool {
	for _, m := range termRE.FindAllStringSubmatch(filter, -1) {
		field, op, want := m[1], m[2], m[3]
		var got string
		if k, ok := strings.CutPrefix(field, "labels."); ok {
			labels, _ := o["labels"].(map[string]any)
			got, _ = labels[k].(string)
		} else {
			got = fmt.Sprint(o[field])
			if o[field] == nil {
				got = ""
			}
		}
		ok := false
		switch op {
		case "=":
			ok = got == want
		case "!=":
			ok = got != want
		case ">":
			ok = got > want
		case "<":
			ok = got < want
		}
		if !ok {
			return false
		}
	}
	return true
}
