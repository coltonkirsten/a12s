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
	"os"
	"path/filepath"
	"time"
)

// CA is the Fleet's built-in certificate authority (§6 "Instance identity").
// Its key lives on the Controller's state volume; leaf certificates are
// issued in memory and never written by the Controller.
type CA struct {
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

// LoadCA reads the CA from dir, creating it on first use.
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
	return newCA(cert, key, certPEM), nil
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
	return newCA(cert, key, certPEM), nil
}

func newCA(cert *x509.Certificate, key *ecdsa.PrivateKey, certPEM []byte) *CA {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{cert: cert, key: key, certPEM: certPEM, pool: pool}
}

// CertPEM is the CA certificate, as injected into /run/a12s/tls/ca.crt.
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// Pool trusts exactly this CA.
func (ca *CA) Pool() *x509.CertPool { return ca.pool }

// Leaf is an issued certificate and its private key, PEM encoded.
type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

// TLS returns the leaf as a tls.Certificate.
func (l Leaf) TLS() (tls.Certificate, error) { return tls.X509KeyPair(l.CertPEM, l.KeyPEM) }

// ComputerCert issues a Computer's certificate: computerd serves with it
// (SAN = the Computer's name) and presents it to the Controller's lease
// listener.
func (ca *CA) ComputerCert(computer string) (Leaf, error) {
	return ca.issue(computer, []string{computer}, x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
}

// ControllerClientCert is what the Controller presents to computerd.
func (ca *CA) ControllerClientCert() (Leaf, error) {
	return ca.issue("controller", nil, x509.ExtKeyUsageClientAuth)
}

// ControllerServerCert serves the Controller's mTLS lease listener under
// host (a12s-F-controller).
func (ca *CA) ControllerServerCert(host string) (Leaf, error) {
	return ca.issue(host, []string{host}, x509.ExtKeyUsageServerAuth)
}

func (ca *CA) issue(cn string, dns []string, usage ...x509.ExtKeyUsage) (Leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Leaf{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dns,
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

// writeFileAtomic writes data to path through a temporary file and rename.
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

// ClientTLS is the Controller's TLS config toward computerd.
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

// ServerTLS is the Controller's mTLS listener config: served as host, and
// every client must present a certificate issued by this CA.
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
		ClientCAs:    ca.pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
