package boundary

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"testing"
)

// The supervisor verifies the leaf against the pinned per-session CA with
// SNI = ServerName over TLS 1.3 / ALPN h2; reproduce that handshake.
func TestTLSMaterialHandshake(t *testing.T) {
	m, err := NewTLSMaterial("0b6f1d7e-9d1c-4a3e-9a52-7c2a3c1f5e10")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair([]byte(m.CertificateChainPEM), []byte(m.PrivateKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); io.WriteString(c, "ok"); c.Close() }()
		}
	}()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(m.TrustAnchorPEM)) {
		t.Fatal("trust anchor does not parse")
	}
	raw, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := tls.Client(raw, &tls.Config{
		RootCAs: pool, ServerName: m.ServerName, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"},
	})
	if err := c.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if st := c.ConnectionState(); st.NegotiatedProtocol != "h2" || st.Version != tls.VersionTLS13 {
		t.Fatalf("negotiated %q / %x", st.NegotiatedProtocol, st.Version)
	}

	// A different session's name must not verify.
	raw2, _ := net.Dial("tcp", l.Addr().String())
	bad := tls.Client(raw2, &tls.Config{RootCAs: pool, ServerName: ServerName("other"), MinVersion: tls.VersionTLS13})
	if err := bad.Handshake(); err == nil {
		t.Fatal("certificate must be pinned to its session name")
	}
}
