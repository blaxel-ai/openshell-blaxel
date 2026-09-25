// Package boundary builds the files OpenShell's Sandbox Protocol needs on each
// side of a sandbox boundary: the openshell-sandbox bootstrap (BoundaryConfig)
// and the openshell-supervisor runtime descriptor, plus their per-session TLS
// identity. Formats mirror crates/openshell-sandbox-backend/src/boundary_protocol.rs
// in NVIDIA/OpenShell; both binaries decode them with deny_unknown_fields.
package boundary

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// TLSMaterial is a fresh, server-only TLS identity for one sandbox session.
// The supervisor pins TrustAnchorPEM and verifies ServerName; the sandbox
// serves CertificateChainPEM with PrivateKeyPEM.
type TLSMaterial struct {
	ServerName          string
	TrustAnchorPEM      string
	CertificateChainPEM string
	PrivateKeyPEM       string
}

// ServerName is the name the supervisor verifies for a session.
func ServerName(sessionID string) string {
	return fmt.Sprintf("sandbox.%s.openshell.internal", sessionID)
}

// NewTLSMaterial mirrors generate_sandbox_tls_material: an Ed25519 CA whose
// private key is discarded after signing one Ed25519 server leaf. Validity is
// deliberately unbounded (1975..4096); the JWT, not certificate time, governs
// authorization.
func NewTLSMaterial(sessionID string) (*TLSMaterial, error) {
	notBefore := time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := time.Date(4096, 1, 1, 0, 0, 0, 0, time.UTC)
	name := ServerName(sessionID)

	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate sandbox CA key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "OpenShell sandbox session CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caPub, caKey)
	if err != nil {
		return nil, fmt.Errorf("generate sandbox CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	leafPub, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate sandbox TLS server key: %w", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "OpenShell sandbox runtime"},
		DNSNames:     []string{name},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, leafPub, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign sandbox TLS server certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return nil, err
	}
	return &TLSMaterial{
		ServerName:          name,
		TrustAnchorPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		CertificateChainPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})),
		PrivateKeyPEM:       string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}
