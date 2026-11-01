package a2a

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLSConfig is the [a2a] mTLS block. mTLS proves the connection, the token
// proves scopes: orthogonal layers, so an mTLS connection still presents a
// token and a token alone still works on a loopback bind. All three files are
// required or omitted together; a partial trio is a config error, not a silent
// downgrade to one-way TLS.
type TLSConfig struct {
	CertFile     string
	KeyFile      string
	ClientCAFile string
}

// Configured reports whether any mTLS field is set, so the wiring can tell
// "operator asked for mTLS" from "plain dev transport".
func (c TLSConfig) Configured() bool {
	return c.CertFile != "" || c.KeyFile != "" || c.ClientCAFile != ""
}

// ServerTLSConfig builds the TLS config for the A2A listener from the mTLS
// trio: the node's own certificate and key, plus the client CA bundle it
// requires every caller's certificate to chain to (RequireAndVerifyClientCert).
// TLS 1.2 is the floor. A partial config is refused so a missing client CA can
// never silently leave the listener accepting any client.
func (c TLSConfig) ServerTLSConfig() (*tls.Config, error) {
	if c.CertFile == "" || c.KeyFile == "" || c.ClientCAFile == "" {
		return nil, fmt.Errorf("%w: cert=%q key=%q client_ca=%q",
			ErrTLSConfigIncomplete, c.CertFile, c.KeyFile, c.ClientCAFile)
	}

	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load a2a server keypair: %w", err)
	}

	clientCAs, err := loadCertPool(c.ClientCAFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig builds the TLS config the local HTTP gateway dials the mTLS
// listener with. The node presents its own certificate and trusts the
// same CA bundle, so the loopback hop passes the listener's
// RequireAndVerifyClientCert. ServerName is left to the dial target: the
// gateway dials by loopback address, so the node's own cert must satisfy
// hostname verification against the connection IP.
func (c TLSConfig) ClientTLSConfig() (*tls.Config, error) {
	if c.CertFile == "" || c.KeyFile == "" || c.ClientCAFile == "" {
		return nil, fmt.Errorf("%w: cert=%q key=%q client_ca=%q",
			ErrTLSConfigIncomplete, c.CertFile, c.KeyFile, c.ClientCAFile)
	}

	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load a2a client keypair: %w", err)
	}

	roots, err := loadCertPool(c.ClientCAFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// loadCertPool reads a PEM bundle into an x509 pool, failing when the file
// holds no certificates so a typo'd path cannot produce an empty trust store
// that rejects every client.
func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) // #nosec G304 -- the CA path is the operator's own [a2a] config
	if err != nil {
		return nil, fmt.Errorf("read a2a client CA file: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: %s", ErrClientCAUnreadable, path)
	}

	return pool, nil
}
