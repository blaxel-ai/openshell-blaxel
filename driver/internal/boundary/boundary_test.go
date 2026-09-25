package boundary

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const pemText = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA\n-----END PUBLIC KEY-----\n"

func sampleAuth(t *testing.T) []byte {
	t.Helper()
	ints := make([]int, len(pemText))
	for i, c := range []byte(pemText) {
		ints[i] = int(c)
	}
	b, _ := json.Marshal(map[string]any{
		"supervisor": map[string]any{
			"session_id": "7c9e6679-7425-40de-944b-e07fc1f90ae7", "runtime_generation": "3b2f1c7e-5d0a-4f7e-9a51-0c1d2e3f4a5b",
			"session_rotation": 1, "auth_epoch": 2, "gateway_token": "eyJ.a.b", "gateway_expires_at": 0,
			"sandbox_token": "eyJ.c.d", "sandbox_expires_at": 0,
		},
		"gateway_id":        "openshell",
		"verification_keys": []map[string]any{{"key_id": "a1b2c3", "public_key_pem": ints}},
	})
	return b
}

func TestDecodeLaunchAuth(t *testing.T) {
	la, err := DecodeLaunchAuth(sampleAuth(t))
	if err != nil {
		t.Fatal(err)
	}
	if la.Session.AuthEpoch != 2 || la.Session.RuntimeGeneration != "3b2f1c7e-5d0a-4f7e-9a51-0c1d2e3f4a5b" {
		t.Fatalf("session = %+v", la.Session)
	}
	if got, _ := la.VerificationKeys[0].PublicKeyPEMString(); got != pemText {
		t.Fatalf("pem = %q", got)
	}
	// The supervisor bundle must pass through byte-identical (tokens included).
	if !strings.Contains(string(la.Supervisor), `"gateway_token":"eyJ.a.b"`) {
		t.Fatalf("supervisor bundle altered: %s", la.Supervisor)
	}

	bad := strings.Replace(string(sampleAuth(t)), `"gateway_id"`, `"unexpected":1,"gateway_id"`, 1)
	if _, err := DecodeLaunchAuth([]byte(bad)); err == nil {
		t.Fatal("unknown fields must be rejected (deny_unknown_fields)")
	}
	if _, err := DecodeLaunchAuth(nil); err == nil {
		t.Fatal("empty launch_authentication must fail")
	}
}

func TestOuterFenceDigest(t *testing.T) {
	gen := "3b2f1c7e-5d0a-4f7e-9a51-0c1d2e3f4a5b"
	ev := FenceEvidence(gen)
	f, err := NewOuterFence(gen, ev)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(gen)))
	h.Write(append(append(n[:], gen...), ev...))
	if f.EvidenceDigest != hex.EncodeToString(h.Sum(nil)) || len(f.EvidenceDigest) != 64 {
		t.Fatalf("digest %s", f.EvidenceDigest)
	}
	want := []string{"default_deny_egress", "no_unmanaged_egress_path", "revocation_verified", "controller_loss_fails_closed"}
	if !reflect.DeepEqual(f.Established, want) {
		t.Fatalf("established = %v", f.Established)
	}
}

func TestBuildLaunchSidesAgree(t *testing.T) {
	la, err := DecodeLaunchAuth(sampleAuth(t))
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewWorkloadIdentity(1500, 1500, "blaxel-config", "sha256:00")
	if err != nil {
		t.Fatal(err)
	}
	l, err := BuildLaunch(LaunchParams{SandboxID: "sb-1", Auth: la, Identity: id,
		ResourceClaims: map[string]string{"blaxel.sandbox": "os-x"},
		SocketPath:     "/run/openshell-boundary/control.sock", CertPath: "/.openshell/state/sandbox.crt",
		KeyPath: "/.openshell/state/sandbox.key", TransportAddr: "127.0.0.1:40123"})
	if err != nil {
		t.Fatal(err)
	}
	c, d := l.Config, l.Descriptor
	if c.Generation != d.Generation || c.SessionID != d.SessionID || !reflect.DeepEqual(c.OuterFence, d.OuterFence) ||
		!reflect.DeepEqual(c.ResourceClaims, d.ResourceClaims) || !reflect.DeepEqual(c.WorkloadIdentity, d.WorkloadIdentity) {
		t.Fatal("BoundaryConfig and descriptor disagree")
	}
	if c.Listener.Kind != "unix" || d.Transport.Kind != "tcp" || d.TLS.ServerName != ServerName(c.SessionID) {
		t.Fatalf("listener/transport: %+v %+v", c.Listener, d.Transport)
	}
	// Fields the Rust side requires must be present even when empty.
	raw, _ := json.Marshal(c)
	for _, k := range []string{`"resource_claim_files":{}`, `"child_env":{}`, `"supplementary_gids":[]`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("BoundaryConfig JSON missing %s: %s", k, raw)
		}
	}
	if strings.Contains(string(raw), "eyJ") {
		t.Fatal("tokens must never reach the sandbox bootstrap")
	}
}

func TestWorkloadIdentityRules(t *testing.T) {
	if _, err := NewWorkloadIdentity(0, 1500, "x", "y"); err == nil {
		t.Error("uid 0 must be rejected")
	}
	if _, err := NewWorkloadIdentity(1500, 1500, " ", "y"); err == nil {
		t.Error("blank source must be rejected")
	}
}
