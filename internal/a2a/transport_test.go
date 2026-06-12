package a2a_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/a2a"
)

// certPEMType and keyPEMType are the PEM block labels the test writers emit.
const (
	certPEMType = "CERTIFICATE"
	keyPEMType  = "PRIVATE KEY"
)

// certValidity bounds the throwaway certs the test mints.
const certValidity = time.Hour

// writeSelfSigned mints one self-signed ed25519 certificate plus its key,
// writes both as PEM under dir, and returns the cert path then the key path.
// The same cert doubles as the CA bundle in these tests: a self-signed cert is
// its own issuer, which is all the credential construction needs to verify.
func writeSelfSigned(t *testing.T, dir, name string) (string, string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate cert key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	certPath := filepath.Join(dir, name+"-cert.pem")
	keyPath := filepath.Join(dir, name+"-key.pem")

	writePEM(t, certPath, certPEMType, der)
	writePEM(t, keyPath, keyPEMType, keyDER)

	return certPath, keyPath
}

// writePEM encodes one DER block as PEM at path.
func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()

	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestTLSConfigConfigured(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  a2a.TLSConfig
		want bool
	}{
		{name: "empty", cfg: a2a.TLSConfig{}, want: false},
		{name: "cert only", cfg: a2a.TLSConfig{CertFile: "c"}, want: true},
		{name: "full trio", cfg: a2a.TLSConfig{CertFile: "c", KeyFile: "k", ClientCAFile: "ca"}, want: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.cfg.Configured(); got != tt.want {
				t.Fatalf("Configured() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestServerCredentialsFromFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "server")

	cfg := a2a.TLSConfig{CertFile: certPath, KeyFile: keyPath, ClientCAFile: certPath}

	creds, err := cfg.ServerCredentials()
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}

	if creds == nil {
		t.Fatal("ServerCredentials returned nil creds without an error")
	}

	if creds.Info().SecurityProtocol != "tls" {
		t.Fatalf("security protocol = %q, want tls", creds.Info().SecurityProtocol)
	}
}

func TestClientCredentialsFromFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "client")

	cfg := a2a.TLSConfig{CertFile: certPath, KeyFile: keyPath, ClientCAFile: certPath}

	creds, err := cfg.ClientCredentials()
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}

	if creds == nil {
		t.Fatal("ClientCredentials returned nil creds without an error")
	}
}

func TestServerCredentialsRejectsPartialConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "partial")

	cases := []struct {
		name string
		cfg  a2a.TLSConfig
	}{
		{name: "missing client CA", cfg: a2a.TLSConfig{CertFile: certPath, KeyFile: keyPath}},
		{name: "missing key", cfg: a2a.TLSConfig{CertFile: certPath, ClientCAFile: certPath}},
		{name: "missing cert", cfg: a2a.TLSConfig{KeyFile: keyPath, ClientCAFile: certPath}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := tt.cfg.ServerCredentials(); !errors.Is(err, a2a.ErrTLSConfigIncomplete) {
				t.Fatalf("ServerCredentials with %s = %v, want ErrTLSConfigIncomplete", tt.name, err)
			}
		})
	}
}

func TestServerCredentialsRejectsUnreadableCA(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "node")

	garbageCA := filepath.Join(dir, "garbage-ca.pem")
	if err := os.WriteFile(garbageCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write garbage CA: %v", err)
	}

	cfg := a2a.TLSConfig{CertFile: certPath, KeyFile: keyPath, ClientCAFile: garbageCA}

	if _, err := cfg.ServerCredentials(); !errors.Is(err, a2a.ErrClientCAUnreadable) {
		t.Fatalf("ServerCredentials with garbage CA = %v, want ErrClientCAUnreadable", err)
	}
}
