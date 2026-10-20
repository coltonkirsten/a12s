package controller

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver"
)

func serveAPI(t *testing.T, lns []net.Listener) {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok")
	})
	for _, ln := range lns {
		srv := &http.Server{Handler: h, ErrorLog: log.New(io.Discard, "", 0)}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
	}
}

func getStatus(t *testing.T, c *http.Client, url string) (int, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func TestListenAPILoopbackOnly(t *testing.T) {
	ca, err := LoadCA(t.TempDir(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	lns, err := listenAPI("127.0.0.1:0", driver.TLSAPI{}, ca)
	if err != nil {
		t.Fatal(err)
	}
	if len(lns) != 1 {
		t.Fatalf("%d listeners, want the loopback one only", len(lns))
	}
	serveAPI(t, lns)
	if code, err := getStatus(t, http.DefaultClient, "http://"+lns[0].Addr().String()+"/v1/status"); err != nil || code != http.StatusOK {
		t.Fatalf("plain HTTP on loopback: %d, %v", code, err)
	}
}

// The internal listener is TLS with a certificate from the fleet's CA for the Controller's address and DNS name,
// and needs no client certificate; the loopback listener stays plain HTTP.
func TestListenAPIInternalTLS(t *testing.T) {
	ca, err := LoadCA(t.TempDir(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	const hostname = "a12s-lab-controller.us-central1-a.c.proj.internal"
	lns, err := listenAPI("127.0.0.1:0", driver.TLSAPI{Addr: "127.0.0.1:0", Names: []string{"10.20.0.2", hostname}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	if len(lns) != 2 {
		t.Fatalf("%d listeners, want loopback and internal", len(lns))
	}
	serveAPI(t, lns)
	loopback, internal := "http://"+lns[0].Addr().String(), lns[1].Addr().String()

	if code, err := getStatus(t, http.DefaultClient, loopback+"/v1/status"); err != nil || code != http.StatusOK {
		t.Fatalf("plain HTTP on loopback: %d, %v", code, err)
	}
	if code, err := getStatus(t, http.DefaultClient, "http://"+internal+"/v1/status"); err == nil && code == http.StatusOK {
		t.Fatal("plain HTTP answered on the internal listener")
	}

	pinned := x509.NewCertPool()
	pinned.AppendCertsFromPEM(ca.CertPEM())
	for _, name := range []string{"10.20.0.2", hostname} {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pinned, ServerName: name}}}
		if code, err := getStatus(t, c, "https://"+internal+"/v1/status"); err != nil || code != http.StatusOK {
			t.Errorf("TLS as %s, pinning the fleet CA and no client certificate: %d, %v", name, code, err)
		}
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pinned, ServerName: "10.20.0.3"}}}
	if _, err := getStatus(t, c, "https://"+internal+"/v1/status"); err == nil {
		t.Error("the certificate verified for an address it does not name")
	}

	conn, err := tls.Dial("tcp", internal, &tls.Config{RootCAs: pinned, ServerName: "10.20.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	leaf := conn.ConnectionState().PeerCertificates[0]
	if !slices.ContainsFunc(leaf.IPAddresses, func(ip net.IP) bool { return ip.Equal(net.ParseIP("10.20.0.2")) }) || !slices.Contains(leaf.DNSNames, hostname) {
		t.Errorf("SANs: IPs %v, DNS %v", leaf.IPAddresses, leaf.DNSNames)
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Errorf("key usage %v, want server auth only", leaf.ExtKeyUsage)
	}
}

func TestListenAPIInternalAddressTaken(t *testing.T) {
	ca, err := LoadCA(t.TempDir(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if _, err := listenAPI("127.0.0.1:0", driver.TLSAPI{Addr: taken.Addr().String(), Names: []string{"10.20.0.2"}}, ca); err == nil {
		t.Fatal("listenAPI succeeded on a taken internal address")
	}
}
