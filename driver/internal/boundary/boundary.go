package boundary

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ---- launch authentication (crates/openshell-core/src/jwt.rs) ----

// LaunchAuth is SandboxLaunchAuthentication: the gateway-minted launch
// credentials for one generation. Supervisor is kept verbatim: it is written
// unchanged to the supervisor's --auth-bundle-file, which strict-decodes it.
type LaunchAuth struct {
	Supervisor       json.RawMessage   `json:"supervisor"`
	GatewayID        string            `json:"gateway_id"`
	VerificationKeys []VerificationKey `json:"verification_keys"`

	// Decoded from Supervisor; never serialized back.
	Session SupervisorSession `json:"-"`
}

// VerificationKey is SessionVerificationKey. Rust serializes Vec<u8> as a
// JSON array of integers, not base64.
type VerificationKey struct {
	KeyID        string `json:"key_id"`
	PublicKeyPEM []int  `json:"public_key_pem"`
}

// SupervisorSession is the non-secret part of SupervisorAuthBundle the driver
// needs; tokens stay inside the raw bundle and never reach the workload.
type SupervisorSession struct {
	SessionID         string `json:"session_id"`
	RuntimeGeneration string `json:"runtime_generation"`
	SessionRotation   uint64 `json:"session_rotation"`
	AuthEpoch         uint64 `json:"auth_epoch"`
}

var (
	uuidRe       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	generationRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

// ValidGeneration reports whether g is a SandboxGenerationId.
func ValidGeneration(g string) bool { return generationRe.MatchString(g) }

// DecodeLaunchAuth parses and validates launch_authentication the way
// SandboxLaunchAuthentication::validate does.
func DecodeLaunchAuth(raw []byte) (*LaunchAuth, error) {
	if len(raw) == 0 {
		return nil, errors.New("launch_authentication is empty (is [openshell.gateway.gateway_jwt] configured?)")
	}
	var la LaunchAuth
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&la); err != nil {
		return nil, fmt.Errorf("decode launch_authentication: %w", err)
	}
	if err := json.Unmarshal(la.Supervisor, &la.Session); err != nil {
		return nil, fmt.Errorf("decode supervisor bundle: %w", err)
	}
	s := la.Session
	switch {
	case !uuidRe.MatchString(s.SessionID) || s.SessionID == "00000000-0000-0000-0000-000000000000":
		return nil, fmt.Errorf("invalid session_id %q", s.SessionID)
	case !ValidGeneration(s.RuntimeGeneration):
		return nil, fmt.Errorf("invalid runtime_generation %q", s.RuntimeGeneration)
	case s.SessionRotation < 1 || s.AuthEpoch < 1:
		return nil, errors.New("session_rotation and auth_epoch must be >= 1")
	case strings.TrimSpace(la.GatewayID) == "":
		return nil, errors.New("gateway_id is empty")
	case len(la.VerificationKeys) == 0:
		return nil, errors.New("no verification keys")
	}
	seen := map[string]bool{}
	for _, k := range la.VerificationKeys {
		if k.KeyID == "" || strings.ContainsAny(k.KeyID, " \t\r\n") || seen[k.KeyID] || len(k.PublicKeyPEM) == 0 {
			return nil, fmt.Errorf("invalid verification key %q", k.KeyID)
		}
		seen[k.KeyID] = true
	}
	return &la, nil
}

// PublicKeyPEMString converts the integer-array PEM to text.
func (k VerificationKey) PublicKeyPEMString() (string, error) {
	b := make([]byte, len(k.PublicKeyPEM))
	for i, v := range k.PublicKeyPEM {
		if v < 0 || v > 255 {
			return "", fmt.Errorf("verification key %q: byte %d out of range", k.KeyID, v)
		}
		b[i] = byte(v)
	}
	return string(b), nil
}

// ---- isolation contract (crates/openshell-isolation-interface/src/contract.rs) ----

// WorkloadIdentity is ResolvedWorkloadIdentity. It must equal the process
// identity openshell-sandbox measures after launch-capability-free
// (setgroups(0), so no supplementary groups).
type WorkloadIdentity struct {
	UID               uint32   `json:"uid"`
	GID               uint32   `json:"gid"`
	SupplementaryGIDs []uint32 `json:"supplementary_gids"`
	Source            string   `json:"source"`
	ResourceDigest    string   `json:"resource_digest"`
}

// NewWorkloadIdentity mirrors ResolvedWorkloadIdentity::new.
func NewWorkloadIdentity(uid, gid uint32, source, digest string) (WorkloadIdentity, error) {
	if uid == 0 || gid == 0 {
		return WorkloadIdentity{}, errors.New("workload uid and gid must be non-zero")
	}
	if strings.TrimSpace(source) == "" || strings.TrimSpace(digest) == "" {
		return WorkloadIdentity{}, errors.New("workload identity source and digest are required")
	}
	return WorkloadIdentity{UID: uid, GID: gid, SupplementaryGIDs: []uint32{}, Source: source, ResourceDigest: digest}, nil
}

// Outer-fence guarantees, serialized snake_case in declaration order.
var allGuarantees = []string{
	"default_deny_egress",
	"no_unmanaged_egress_path",
	"revocation_verified",
	"controller_loss_fails_closed",
}

// OuterFence is OuterFenceGuarantees: the driver's projection of the network
// fence it established for one generation.
type OuterFence struct {
	Generation     string   `json:"generation"`
	Established    []string `json:"established"`
	EvidenceDigest string   `json:"evidence_digest"`
}

// NewOuterFence mirrors OuterFenceGuarantees::from_enforcement_evidence with
// all four guarantees: digest = SHA256(u64_be(len(gen)) || gen || evidence).
func NewOuterFence(generation string, evidence []byte) (OuterFence, error) {
	if generation == "" || len(evidence) == 0 {
		return OuterFence{}, errors.New("fence generation and evidence are required")
	}
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(generation)))
	h.Write(n[:])
	h.Write([]byte(generation))
	h.Write(evidence)
	return OuterFence{Generation: generation, Established: append([]string(nil), allGuarantees...),
		EvidenceDigest: hex.EncodeToString(h.Sum(nil))}, nil
}

// FenceEvidence is the driver-native evidence for the Blaxel placement: the
// workload runs in a network namespace whose only interface is loopback.
func FenceEvidence(generation string) []byte {
	b, _ := json.Marshal(struct {
		Generation       string   `json:"generation"`
		NetworkNamespace string   `json:"network_namespace"`
		Interfaces       []string `json:"interfaces"`
	}{generation, "private", []string{"lo"}})
	return b
}

// ---- boundary protocol (crates/openshell-sandbox-backend/src/boundary_protocol.rs) ----

type TLSServerFiles struct {
	CertificateChainPath string `json:"certificate_chain_path"`
	PrivateKeyPath       string `json:"private_key_path"`
}

// Listener is BoundaryListener::Unix (tag "kind").
type Listener struct {
	Kind       string         `json:"kind"`
	SocketPath string         `json:"socket_path"`
	TLS        TLSServerFiles `json:"tls"`
}

type GatewayVerificationKey struct {
	KeyID        string `json:"key_id"`
	PublicKeyPEM string `json:"public_key_pem"`
}

// Config is BoundaryConfig, the openshell-sandbox bootstrap.
type Config struct {
	BoundaryID         string                   `json:"boundary_id"`
	Generation         string                   `json:"generation"`
	SessionID          string                   `json:"session_id"`
	SessionRotation    uint64                   `json:"session_rotation"`
	AuthEpoch          uint64                   `json:"auth_epoch"`
	GatewayID          string                   `json:"gateway_id"`
	VerificationKeys   []GatewayVerificationKey `json:"verification_keys"`
	Listener           Listener                 `json:"listener"`
	ResourceClaims     map[string]string        `json:"resource_claims"`
	ResourceClaimFiles map[string]string        `json:"resource_claim_files"`
	WorkloadIdentity   WorkloadIdentity         `json:"workload_identity"`
	OuterFence         OuterFence               `json:"outer_fence"`
	ChildEnv           map[string]string        `json:"child_env"`
}

// Transport is SandboxTransport::Tcp (tag "kind").
type Transport struct {
	Kind      string   `json:"kind"`
	Authority string   `json:"authority"`
	Addresses []string `json:"addresses"`
}

type TLSClient struct {
	ServerName     string `json:"server_name"`
	TrustAnchorPEM string `json:"trust_anchor_pem"`
}

// Descriptor is SandboxRuntimeDescriptor, the file behind the supervisor's
// --backend-descriptor-file (raw JSON, no envelope).
type Descriptor struct {
	BoundaryID       string            `json:"boundary_id"`
	Generation       string            `json:"generation"`
	SessionID        string            `json:"session_id"`
	WorkloadIdentity WorkloadIdentity  `json:"workload_identity"`
	Transport        Transport         `json:"transport"`
	TLS              TLSClient         `json:"tls"`
	HostGatewayIP    *string           `json:"host_gateway_ip"`
	ResourceClaims   map[string]string `json:"resource_claims"`
	OuterFence       OuterFence        `json:"outer_fence"`
}

// Launch is everything one generation needs on both sides of the boundary.
type Launch struct {
	Config        Config
	Descriptor    Descriptor
	TLS           *TLSMaterial
	SupervisorRaw json.RawMessage
}

// LaunchParams are the driver-side inputs for BuildLaunch.
type LaunchParams struct {
	SandboxID      string
	Auth           *LaunchAuth
	Identity       WorkloadIdentity
	ResourceClaims map[string]string
	SocketPath     string // in-VM Unix listener
	CertPath       string // in-VM TLS chain
	KeyPath        string // in-VM TLS key
	TransportAddr  string // supervisor-side 127.0.0.1:port
	ChildEnv       map[string]string
}

// BuildLaunch produces a matching BoundaryConfig and runtime descriptor: the
// sandbox and supervisor compare generation, session, identity, claims and
// fence, so every shared value comes from one place.
func BuildLaunch(p LaunchParams) (*Launch, error) {
	s := p.Auth.Session
	for k, v := range p.ResourceClaims {
		if k == "" || v == "" || strings.ContainsAny(k+v, " \t\r\n") {
			return nil, fmt.Errorf("resource claim %q=%q must be non-empty without whitespace", k, v)
		}
	}
	tlsm, err := NewTLSMaterial(s.SessionID)
	if err != nil {
		return nil, err
	}
	fence, err := NewOuterFence(s.RuntimeGeneration, FenceEvidence(s.RuntimeGeneration))
	if err != nil {
		return nil, err
	}
	keys := make([]GatewayVerificationKey, 0, len(p.Auth.VerificationKeys))
	for _, k := range p.Auth.VerificationKeys {
		pemText, err := k.PublicKeyPEMString()
		if err != nil {
			return nil, err
		}
		keys = append(keys, GatewayVerificationKey{KeyID: k.KeyID, PublicKeyPEM: pemText})
	}
	claims := map[string]string{}
	for k, v := range p.ResourceClaims {
		claims[k] = v
	}
	childEnv := map[string]string{}
	for k, v := range p.ChildEnv {
		childEnv[k] = v
	}
	loopback := "127.0.0.1"
	return &Launch{
		TLS:           tlsm,
		SupervisorRaw: p.Auth.Supervisor,
		Config: Config{
			BoundaryID: p.SandboxID, Generation: s.RuntimeGeneration, SessionID: s.SessionID,
			SessionRotation: s.SessionRotation, AuthEpoch: s.AuthEpoch, GatewayID: p.Auth.GatewayID,
			VerificationKeys: keys,
			Listener: Listener{Kind: "unix", SocketPath: p.SocketPath,
				TLS: TLSServerFiles{CertificateChainPath: p.CertPath, PrivateKeyPath: p.KeyPath}},
			ResourceClaims: claims, ResourceClaimFiles: map[string]string{},
			WorkloadIdentity: p.Identity, OuterFence: fence, ChildEnv: childEnv,
		},
		Descriptor: Descriptor{
			BoundaryID: p.SandboxID, Generation: s.RuntimeGeneration, SessionID: s.SessionID,
			WorkloadIdentity: p.Identity,
			Transport:        Transport{Kind: "tcp", Authority: "blaxel/" + p.SandboxID, Addresses: []string{p.TransportAddr}},
			TLS:              TLSClient{ServerName: tlsm.ServerName, TrustAnchorPEM: tlsm.TrustAnchorPEM},
			HostGatewayIP:    &loopback,
			ResourceClaims:   claims,
			OuterFence:       fence,
		},
	}, nil
}
