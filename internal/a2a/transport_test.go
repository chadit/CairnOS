package a2a_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
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
	"github.com/chadit/CairnOS/internal/api"
)

// certPEMType and keyPEMType are the PEM block labels the test writers emit.
const (
	certPEMType = "CERTIFICATE"
	keyPEMType  = "PRIVATE KEY"
)

// tlsProtocol is the SecurityProtocol gRPC reports for TLS credentials.
const tlsProtocol = "tls"

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

func TestServerTLSConfigFromFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "server")

	cfg := a2a.TLSConfig{CertFile: certPath, KeyFile: keyPath, ClientCAFile: certPath}

	serverTLS, err := cfg.ServerTLSConfig()
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	if serverTLS.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("client auth = %v, want RequireAndVerifyClientCert", serverTLS.ClientAuth)
	}

	if serverTLS.ClientCAs == nil || len(serverTLS.Certificates) != 1 {
		t.Fatalf("server TLS has client CAs %v and %d certificates, want a pool and one", serverTLS.ClientCAs, len(serverTLS.Certificates))
	}

	if serverTLS.MinVersion != tls.VersionTLS12 {
		t.Fatalf("min version = %#x, want TLS 1.2", serverTLS.MinVersion)
	}
}

func TestClientTLSConfigFromFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "client")

	cfg := a2a.TLSConfig{CertFile: certPath, KeyFile: keyPath, ClientCAFile: certPath}

	clientTLS, err := cfg.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}

	if clientTLS.RootCAs == nil || len(clientTLS.Certificates) != 1 {
		t.Fatalf("client TLS has roots %v and %d certificates, want a pool and one", clientTLS.RootCAs, len(clientTLS.Certificates))
	}
}

func TestServerTLSConfigRejectsPartialConfig(t *testing.T) {
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

			if _, err := tt.cfg.ServerTLSConfig(); !errors.Is(err, a2a.ErrTLSConfigIncomplete) {
				t.Fatalf("ServerTLSConfig with %s = %v, want ErrTLSConfigIncomplete", tt.name, err)
			}
		})
	}
}

func TestServerTLSConfigRejectsUnreadableCA(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "node")

	garbageCA := filepath.Join(dir, "garbage-ca.pem")
	if err := os.WriteFile(garbageCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write garbage CA: %v", err)
	}

	cfg := a2a.TLSConfig{CertFile: certPath, KeyFile: keyPath, ClientCAFile: garbageCA}

	if _, err := cfg.ServerTLSConfig(); !errors.Is(err, a2a.ErrClientCAUnreadable) {
		t.Fatalf("ServerTLSConfig with garbage CA = %v, want ErrClientCAUnreadable", err)
	}
}

// TestBundleWiresMTLSCredentials proves an mTLS block turns into TLS server
// credentials on the bundle and a TLS dial credential for the local gateway.
func TestBundleWiresMTLSCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "bundle")

	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate card key: %v", err)
	}

	bundle, err := a2a.NewBundle(&a2a.BundleConfig{
		NodeName:   nodeName,
		SigningKey: signingKey,
		TLS:        a2a.TLSConfig{CertFile: certPath, KeyFile: keyPath, ClientCAFile: certPath},
	})
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	if bundle.Creds == nil || bundle.Creds.Info().SecurityProtocol != tlsProtocol {
		t.Fatalf("bundle creds = %v, want TLS server credentials", bundle.Creds)
	}

	apiCfg := &api.Config{}
	if err := bundle.Attach(apiCfg); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if apiCfg.GatewayDialCreds == nil || apiCfg.GatewayDialCreds.Info().SecurityProtocol != tlsProtocol {
		t.Fatalf("gateway dial creds = %v, want TLS client credentials", apiCfg.GatewayDialCreds)
	}
}

// TestBundleWithoutTLSStaysPlaintext pins the plaintext default: no mTLS block
// leaves both credentials nil so the API server falls back to insecure.
func TestBundleWithoutTLSStaysPlaintext(t *testing.T) {
	t.Parallel()

	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate card key: %v", err)
	}

	bundle, err := a2a.NewBundle(&a2a.BundleConfig{NodeName: nodeName, SigningKey: signingKey})
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	apiCfg := &api.Config{}
	if err := bundle.Attach(apiCfg); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if bundle.Creds != nil || apiCfg.GatewayDialCreds != nil {
		t.Fatalf("plaintext bundle set creds %v and dial creds %v, want both nil", bundle.Creds, apiCfg.GatewayDialCreds)
	}
}
