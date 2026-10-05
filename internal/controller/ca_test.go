package controller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/computerd"
)

func TestCAPersistsWithOwnerOnlyFiles(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadCA(dir, "lab")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{caCertFile, caKeyFile} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, st.Mode(), err)
		}
	}
	again, err := LoadCA(dir, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if string(again.CertPEM()) != string(ca.CertPEM()) {
		t.Fatal("CA changed on reload")
	}
}

// startComputerd serves a computerd-style mTLS endpoint with a Computer's
// certificate, requiring a client certificate from the CA.
func startComputerd(t *testing.T, ca *CA, name string, h http.Handler) *httptest.Server {
	t.Helper()
	leaf, err := ca.ComputerCert(name)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := leaf.TLS()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: ca.Pool(), ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestCARoundTrip(t *testing.T) {
	ca, err := LoadCA(t.TempDir(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	var peer string
	srv := startComputerd(t, ca, "scout", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer = r.TLS.PeerCertificates[0].Subject.CommonName
		json.NewEncoder(w).Encode(api.ComputerdStatus{Phase: "ready", Session: "s_1"})
	}))
	dial := func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}
	clientTLS, err := ca.ClientTLS()
	if err != nil {
		t.Fatal(err)
	}

	st, err := computerd.New("scout", dial, clientTLS).Status(context.Background())
	if err != nil || st.Phase != "ready" || peer != "controller" {
		t.Fatalf("status = %+v, %v (peer %q)", st, err, peer)
	}
	// The server name is verified: scout's certificate does not serve tester.
	if _, err := computerd.New("tester", dial, clientTLS).Status(context.Background()); err == nil {
		t.Fatal("tester accepted scout's certificate")
	}
	// A client certificate from another CA is refused.
	other, _ := LoadCA(t.TempDir(), "lab")
	otherTLS, _ := other.ClientTLS()
	otherTLS.RootCAs = ca.Pool()
	if _, err := computerd.New("scout", dial, otherTLS).Status(context.Background()); err == nil {
		t.Fatal("foreign client certificate accepted")
	}
}

func TestComputerCertIsALeaseClient(t *testing.T) {
	ca, _ := LoadCA(t.TempDir(), "lab")
	serverTLS, err := ca.ServerTLS("a12s-lab-controller")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.TLS.PeerCertificates[0].DNSNames[0]))
	}))
	srv.TLS = serverTLS
	srv.StartTLS()
	defer srv.Close()

	leaf, _ := ca.ComputerCert("scout")
	cert, _ := leaf.TLS()
	tr := &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: ca.Pool(), ServerName: "a12s-lab-controller"}}
	res, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var buf [16]byte
	n, _ := res.Body.Read(buf[:])
	if string(buf[:n]) != "scout" {
		t.Fatalf("peer = %q", buf[:n])
	}
}
