package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CA's key lives on the Controller's state volume; leaf certificates are issued in memory
// and never written by the Controller.
type CA struct {
	fleet   string
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	pool    *x509.CertPool
}

const (
	caCertFile = "ca.crt"
	caKeyFile  = "ca.key"
	caLifetime = 10 * 365 * 24 * time.Hour
	leafLife   = 2 * 365 * 24 * time.Hour
)

func LoadCA(dir, fleet string) (*CA, error) {
	certPEM, err1 := os.ReadFile(filepath.Join(dir, caCertFile))
	keyPEM, err2 := os.ReadFile(filepath.Join(dir, caKeyFile))
	if errors.Is(err1, fs.ErrNotExist) || errors.Is(err2, fs.ErrNotExist) {
		return createCA(dir, fleet)
	}
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("load CA: ca.key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("load CA: ca.key is not an ECDSA key")
	}
	ca := newCA(cert, key, certPEM)
	ca.fleet = fleet
	return ca, nil
}

func createCA(dir, fleet string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "a12s " + fleet + " CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFileAtomic(filepath.Join(dir, caKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, fmt.Errorf("create CA: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, caCertFile), certPEM, 0o600); err != nil {
		return nil, fmt.Errorf("create CA: %w", err)
	}
	ca := newCA(cert, key, certPEM)
	ca.fleet = fleet
	return ca, nil
}

func newCA(cert *x509.Certificate, key *ecdsa.PrivateKey, certPEM []byte) *CA {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{cert: cert, key: key, certPEM: certPEM, pool: pool}
}

// CertPEM is the CA certificate, as injected into /run/a12s/tls/ca.crt.
func (ca *CA) CertPEM() []byte { return ca.certPEM }

func (ca *CA) Pool() *x509.CertPool { return ca.pool }

type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
	// Serial (hex) is the key in the active-certificate registry.
	Serial string
}

func (l Leaf) TLS() (tls.Certificate, error) { return tls.X509KeyPair(l.CertPEM, l.KeyPEM) }

// Only the Controller's certificate carries client-auth usage; computerd requires both.
const ControllerCN = "controller"

// ComputerCert has no client-auth usage, so a Computer cannot call another computerd;
// only the Controller's mTLS listener accepts it from a client (see ServerTLS).
func (ca *CA) ComputerCert(computer string, generation int64) (Leaf, error) {
	return ca.issueTyped(computer, []string{computer}, Principal{Kind: KindComputer, Name: computer, Generation: generation})
}

// ServiceCert has a typed URI SAN and no DNS name: a Service serves nothing a12s dials over TLS.
func (ca *CA) ServiceCert(service string, generation int64) (Leaf, error) {
	return ca.issueTyped(service, nil, Principal{Kind: KindService, Name: service, Generation: generation})
}

const (
	KindComputer = "computer"
	KindService  = "service"
)

// Principal is the typed identity in a certificate's URI SAN:
// a12s://<fleet>/<kind>/<name>/<generation>.
type Principal struct {
	Kind       string
	Name       string
	Generation int64
}

// String is "kind:name", the via of the Messages it sends.
func (p Principal) String() string { return p.Kind + ":" + p.Name }

func (ca *CA) principalURI(p Principal) *url.URL {
	return &url.URL{Scheme: "a12s", Host: ca.fleet, Path: "/" + p.Kind + "/" + p.Name + "/" + strconv.FormatInt(p.Generation, 10)}
}

func (ca *CA) PrincipalOf(leaf *x509.Certificate) (Principal, error) {
	if len(leaf.URIs) != 1 {
		return Principal{}, errors.New("certificate has no a12s identity")
	}
	u := leaf.URIs[0]
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if u.Scheme != "a12s" || u.Host != ca.fleet || len(parts) != 3 {
		return Principal{}, fmt.Errorf("certificate identity %s is not of fleet %s", u, ca.fleet)
	}
	gen, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || (parts[0] != KindComputer && parts[0] != KindService) || parts[1] == "" {
		return Principal{}, fmt.Errorf("certificate identity %s is malformed", u)
	}
	return Principal{Kind: parts[0], Name: parts[1], Generation: gen}, nil
}

func (ca *CA) issueTyped(cn string, dns []string, p Principal) (Leaf, error) {
	return ca.issueURI(cn, dns, []*url.URL{ca.principalURI(p)}, x509.ExtKeyUsageServerAuth)
}

func (ca *CA) ControllerClientCert() (Leaf, error) {
	return ca.issue(ControllerCN, nil, x509.ExtKeyUsageClientAuth)
}

func (ca *CA) ControllerServerCert(host string) (Leaf, error) {
	return ca.issue(host, []string{host}, x509.ExtKeyUsageServerAuth)
}

func (ca *CA) issue(cn string, dns []string, usage ...x509.ExtKeyUsage) (Leaf, error) {
	return ca.issueURI(cn, dns, nil, usage...)
}

func (ca *CA) issueURI(cn string, dns []string, uris []*url.URL, usage ...x509.ExtKeyUsage) (Leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Leaf{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dns,
		URIs:         uris,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usage,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return Leaf{}, fmt.Errorf("issue %s: %w", cn, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Leaf{}, err
	}
	return Leaf{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Serial:  tmpl.SerialNumber.Text(16),
	}, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

func parseCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// writeFileAtomic goes through a temporary file and rename so readers never see a partial file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (ca *CA) ClientTLS() (*tls.Config, error) {
	leaf, err := ca.ControllerClientCert()
	if err != nil {
		return nil, err
	}
	cert, err := leaf.TLS()
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: ca.pool, MinVersion: tls.VersionTLS12}, nil
}

// ServerTLS accepts only Computer certificates issued by this CA; the Controller's own client
// certificate is refused.
func (ca *CA) ServerTLS(host string) (*tls.Config, error) {
	leaf, err := ca.ControllerServerCert(host)
	if err != nil {
		return nil, err
	}
	cert, err := leaf.TLS()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Verified below: the standard client verification demands
		// client-auth usage, which Computer certificates do not carry.
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return ca.verifyComputer(cs.PeerCertificates)
		},
		MinVersion: tls.VersionTLS12,
	}, nil
}

func (ca *CA) verifyComputer(chain []*x509.Certificate) error {
	if len(chain) == 0 {
		return errors.New("no client certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	leaf := chain[0]
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: ca.pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("client certificate: %w", err)
	}
	if _, err := ca.PrincipalOf(leaf); err != nil {
		return fmt.Errorf("client certificate: %w", err)
	}
	return nil
}
