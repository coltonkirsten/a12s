package gcefake

import (
	"context"
	"net/http"
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

func TestAPIAgainstFake(t *testing.T) {
	ctx := context.Background()
	f := New("p", "us-central1-a")
	defer f.Close()
	a := f.API()
	if a.Region() != "us-central1" {
		t.Fatalf("region %q", a.Region())
	}

	if err := a.InsertDisk(ctx, &gceapi.Disk{Name: "home", SizeGb: 10, Labels: map[string]string{"a12s-fleet": "lab"}}); err != nil {
		t.Fatal(err)
	}
	in := &gceapi.Instance{
		Name: "vm", Labels: map[string]string{"a12s-fleet": "lab", "a12s-role": "computer"},
		Disks: []gceapi.AttachedDisk{
			{Boot: true, AutoDelete: true, InitializeParams: &gceapi.InitializeParams{DiskSizeGb: 10}},
			{DeviceName: "home", Source: a.ZoneURL("disks", "home")},
		},
		NetworkInterfaces: []gceapi.NetworkInterface{{Subnetwork: "s"}},
	}
	if err := a.InsertInstance(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := a.InsertInstance(ctx, in); !gceapi.IsConflict(err) {
		t.Fatalf("second insert = %v, want a conflict", err)
	}
	got, err := a.GetInstance(ctx, "vm")
	if err != nil || got.Status != "RUNNING" || got.NetworkInterfaces[0].NetworkIP == "" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := a.GetInstance(ctx, "nope"); !gceapi.IsNotFound(err) {
		t.Fatalf("get missing = %v", err)
	}

	list, err := a.ListInstances(ctx, "(labels.a12s-fleet=lab) (labels.a12s-role=computer)")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if list, _ := a.ListInstances(ctx, "labels.a12s-fleet=other"); len(list) != 0 {
		t.Fatalf("filter ignored: %v", list)
	}

	// A stale fingerprint loses: compare-and-swap is GCE's, not the caller's.
	fp := got.LabelFingerprint
	if err := a.SetLabels(ctx, "vm", map[string]string{"a12s-generation": "2"}, fp); err != nil {
		t.Fatal(err)
	}
	if err := a.SetLabels(ctx, "vm", map[string]string{"a12s-generation": "1"}, fp); !gceapi.IsConflict(err) {
		t.Fatalf("stale setLabels = %v, want a conflict", err)
	}

	if err := a.DeleteDisk(ctx, "home"); !gceapi.HasReason(err, "resourceInUseByAnotherResource") {
		t.Fatalf("delete attached disk = %v", err)
	}

	f.FailOp("start", "vm", "ZONE_RESOURCE_POOL_EXHAUSTED")
	if err := a.StopInstance(ctx, "vm"); err != nil {
		t.Fatal(err)
	}
	if err := a.StartInstance(ctx, "vm"); !gceapi.HasReason(err, "ZONE_RESOURCE_POOL_EXHAUSTED") {
		t.Fatalf("start = %v, want a stockout", err)
	}
	if got, _ := a.GetInstance(ctx, "vm"); got.Status != "TERMINATED" {
		t.Fatalf("a failed start changed the status to %s", got.Status)
	}

	f.FailHTTP("instances.start", "", http.StatusForbidden, "forbidden")
	if err := a.StartInstance(ctx, "vm"); !gceapi.HasReason(err, "forbidden") {
		t.Fatalf("start = %v, want forbidden", err)
	}

	if err := a.DeleteInstance(ctx, "vm"); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.GetDisk(ctx, "home"); len(d.Users) != 0 {
		t.Fatalf("home disk still attached: %v", d.Users)
	}
	if _, ok := f.Disk("vm"); ok {
		t.Fatal("boot disk outlived its instance")
	}

	f.HostError("missing") // no instance: ignored
	ops, err := a.ListOperations(ctx, `insertTime > "2026-10-19T12:00:05Z"`)
	if err != nil || len(ops) == 0 {
		t.Fatalf("operations = %v, %v", ops, err)
	}

	sa, err := a.CreateServiceAccount(ctx, "lab-scout-1a2b", "a12s lab/scout")
	if err != nil || sa.Email != "lab-scout-1a2b@p.iam.gserviceaccount.com" {
		t.Fatalf("create sa = %+v, %v", sa, err)
	}
	if _, err := a.CreateServiceAccount(ctx, "lab-scout-1a2b", ""); !gceapi.IsConflict(err) {
		t.Fatalf("second create = %v", err)
	}
	if err := a.DeleteServiceAccount(ctx, sa.Email); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetServiceAccount(ctx, sa.Email); !gceapi.IsNotFound(err) {
		t.Fatalf("get deleted sa = %v", err)
	}

	f.Deny("compute.instances.create")
	missing, err := a.MissingPermissions(ctx, []string{"compute.instances.create", "compute.instances.get"})
	if err != nil || len(missing) != 1 || missing[0] != "compute.instances.create" {
		t.Fatalf("missing = %v, %v", missing, err)
	}
}
