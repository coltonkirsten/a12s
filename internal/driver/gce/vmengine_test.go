package gce

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"gopkg.in/yaml.v3"

	"github.com/coltonkirsten/a12s/internal/driver/engine"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gcefake"
)

// The engine link trusts exactly the certificate the VM published: another certificate, even a valid one, is refused,
// and the engine sees the Controller's client certificate.
func TestEngineClientPinsTheVMsCertificate(t *testing.T) {
	var sawClientCert bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawClientCert = len(r.TLS.PeerCertificates) == 1
		w.Header().Set("Api-Version", "1.44")
		w.Write([]byte("OK"))
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()
	addr := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "https://"))
	served := srv.TLS.Certificates[0].Certificate[0]
	clientCert := selfSigned(t)

	ping := func(pin []byte) error {
		cli, err := client.New(
			client.WithHost("tcp://"+addr.String()), client.WithScheme("https"),
			client.WithHTTPClient(&http.Client{Transport: pinnedTransport(pin, clientCert)}),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer cli.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = cli.Ping(ctx, client.PingOptions{})
		return err
	}
	if err := ping(served); err != nil || !sawClientCert {
		t.Fatalf("ping with the published certificate = %v (client cert seen: %v)", err, sawClientCert)
	}
	other := selfSigned(t).Certificate[0]
	if err := ping(other); err == nil {
		t.Fatal("an engine with another certificate was trusted")
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A boot script that failed is reported at once, with what failed, instead of waiting out the boot timeout.
func TestOpenReportsABootFailure(t *testing.T) {
	pollEvery = 0
	f := gcefake.New("p", "us-central1-a")
	defer f.Close()
	if err := f.API().InsertInstance(context.Background(), &gceapi.Instance{Name: "vm"}); err != nil {
		t.Fatal(err)
	}
	f.SetGuestAttribute("vm", attrBootError, "line 52: openssl req ...")
	e := &Engines{API: f.API(), Fleet: "lab", Wait: time.Minute}
	start := time.Now()
	_, err := e.Open(context.Background(), VM{Name: "vm", Address: netip.MustParseAddr("10.128.0.9")})
	if err == nil || !strings.Contains(err.Error(), "openssl") || time.Since(start) > 10*time.Second {
		t.Fatalf("Open = %v after %s", err, time.Since(start))
	}
	f.SetGuestAttribute("vm", attrBootError, "none")
	e.Wait = 50 * time.Millisecond
	if _, err := e.Open(context.Background(), VM{Name: "vm", Address: netip.MustParseAddr("10.128.0.9")}); err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Fatalf("Open with no engine certificate = %v", err)
	}
}

func TestLoadedImageID(t *testing.T) {
	got, err := loadedImageID(strings.NewReader(`{"stream":"Loaded image ID: sha256:abc\n"}` + "\n"))
	if err != nil || got != "sha256:abc" {
		t.Fatalf("= %q, %v", got, err)
	}
	if _, err := loadedImageID(strings.NewReader(`{"error":"no space left on device"}` + "\n")); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("an engine error was not reported: %v", err)
	}
}

// The container on a VM runs with the engine package's settings, on the default bridge (no embedded DNS), its home
// on the VM's home disk, computerd published for the Controller, no container limits (the VM is sized to the spec),
// and the Controller reached by address.
func TestVMPlacement(t *testing.T) {
	g := &vmGuest{e: &Engines{Fleet: "lab", Controller: netip.MustParseAddr("10.128.0.2")}, vm: VM{Computer: "scout"}}
	spec := testSpec("scout", 3)
	spec.CPUMillis, spec.Memory = 2000, 8<<30
	o := engine.New(nil, "lab").CreateOptions(spec, g.placement())
	h := o.HostConfig
	if o.Name != "a12s-lab-scout" || h.NetworkMode != "bridge" || h.Memory != 0 || h.NanoCPUs != 0 || !h.ReadonlyRootfs {
		t.Fatalf("name %s network %s memory %d cpus %d ro %v", o.Name, h.NetworkMode, h.Memory, h.NanoCPUs, h.ReadonlyRootfs)
	}
	if h.Mounts[0].Type != mount.TypeBind || h.Mounts[0].Source != HomeDir || h.Mounts[0].Target != "/home" {
		t.Fatalf("home mount %+v", h.Mounts[0])
	}
	if b := h.PortBindings[network.MustParsePort("7443/tcp")]; len(b) != 1 || b[0].HostPort != "7443" {
		t.Fatalf("ports %v", h.PortBindings)
	}
	for _, want := range []string{"A12S_PROXY=http://10.128.0.2:3128", "A12S_CONTROLLER=https://10.128.0.2:7443"} {
		if !slices.Contains(o.Config.Env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}

func TestBootMetadata(t *testing.T) {
	items := BootMetadata(netip.MustParseAddr("10.128.0.2"), []byte("CA PEM"), "none")
	md := map[string]string{}
	for _, it := range items {
		md[it.Key] = it.Value
	}
	if md[mdController] != "10.128.0.2" || md[mdCA] != "CA PEM" || md[mdIdentity] != "none" ||
		md["enable-guest-attributes"] != "TRUE" || md["block-project-ssh-keys"] != "TRUE" {
		t.Fatalf("metadata %v", md)
	}
	var cc struct {
		WriteFiles []struct {
			Path, Content string
		} `yaml:"write_files"`
		Runcmd []string `yaml:"runcmd"`
	}
	ud := md["user-data"]
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("user-data is not cloud-config")
	}
	if err := yaml.Unmarshal([]byte(ud), &cc); err != nil {
		t.Fatal(err)
	}
	if len(cc.WriteFiles) != 1 || cc.WriteFiles[0].Content != bootScript || len(cc.Runcmd) != 1 || !strings.Contains(cc.Runcmd[0], cc.WriteFiles[0].Path) {
		t.Fatalf("cloud-config does not carry and run boot.sh: %+v", cc)
	}
}
