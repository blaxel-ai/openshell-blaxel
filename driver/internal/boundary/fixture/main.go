// fixture writes a Go-built BoundaryConfig + TLS files for a smoke test of
// the real openshell-sandbox binary: go run ./internal/boundary/fixture <dir>
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"

	"github.com/blaxel-ai/openshell-blaxel/driver/internal/boundary"
)

func main() {
	dir := os.Args[1]
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	ints := make([]int, len(pemBytes))
	for i, b := range pemBytes {
		ints[i] = int(b)
	}
	raw, _ := json.Marshal(map[string]any{
		"supervisor": map[string]any{"session_id": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
			"runtime_generation": "3b2f1c7e-5d0a-4f7e-9a51-0c1d2e3f4a5b", "session_rotation": 1, "auth_epoch": 1,
			"gateway_token": "eyJ.x.y", "gateway_expires_at": 0, "sandbox_token": "eyJ.z.w", "sandbox_expires_at": 0},
		"gateway_id": "openshell", "verification_keys": []map[string]any{{"key_id": "fixturekey", "public_key_pem": ints}},
	})
	la, err := boundary.DecodeLaunchAuth(raw)
	if err != nil {
		panic(err)
	}
	id, _ := boundary.NewWorkloadIdentity(1500, 1500, "blaxel-config", "sha256:fixture")
	l, err := boundary.BuildLaunch(boundary.LaunchParams{SandboxID: "fixture-sandbox", Auth: la, Identity: id,
		ResourceClaims: map[string]string{"blaxel.sandbox": "os-wl-probe"},
		SocketPath:     "/run/openshell-boundary/control.sock",
		CertPath:       "/.openshell/state/sandbox.crt", KeyPath: "/.openshell/state/sandbox.key",
		TransportAddr: "127.0.0.1:40123"})
	if err != nil {
		panic(err)
	}
	cfg, _ := json.Marshal(l.Config)
	desc, _ := json.Marshal(l.Descriptor)
	os.WriteFile(filepath.Join(dir, "bootstrap.json"), cfg, 0o600)
	os.WriteFile(filepath.Join(dir, "sandbox.crt"), []byte(l.TLS.CertificateChainPEM), 0o600)
	os.WriteFile(filepath.Join(dir, "sandbox.key"), []byte(l.TLS.PrivateKeyPEM), 0o600)
	os.WriteFile(filepath.Join(dir, "descriptor.json"), desc, 0o600)
}
