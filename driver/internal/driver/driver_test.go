package driver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	pb "github.com/blaxel-ai/openshell-blaxel/driver/gen/computev1"
	extpb "github.com/blaxel-ai/openshell-blaxel/driver/gen/extensionv1"
)

func testDriver() *Driver {
	d := New(Config{DefaultImage: "blaxel/py-app:latest", MemoryMiB: 4096, Owner: DefaultOwner,
		GatewayEndpoint: "https://127.0.0.1:17670", GatewayCA: "/ca", GatewayCert: "/crt", GatewayKey: "/key", LogLevel: "info"}, nil, nil)
	d.log = discardLogger()
	return d
}

func gatewayMeta(major uint32) *extpb.PeerMetadata {
	return &extpb.PeerMetadata{ProtocolVersion: &extpb.ProtocolVersion{Major: major}, ImplementationName: "openshell/gateway",
		ImplementationVersion: "0.0.117", SupportedCapabilities: []string{computeContract}, RequiredCapabilities: []string{computeContract}}
}

func TestCapabilitiesNegotiation(t *testing.T) {
	d := testDriver()
	resp, err := d.GetCapabilities(context.Background(), &pb.GetCapabilitiesRequest{Gateway: gatewayMeta(1)})
	if err != nil {
		t.Fatal(err)
	}
	ext := resp.GetExtension()
	if ext.GetProtocolVersion().GetMajor() != 1 || ext.GetSupportedCapabilities()[0] != computeContract || ext.GetImplementationName() == "" {
		t.Fatalf("extension metadata = %+v", ext)
	}
	if resp.GetResourceAdmissionPolicy() != DefaultAdmissionPolicy || resp.GetDriverReportsRuntimeReadiness() {
		t.Fatal("admission policy must be the default acknowledgement; readiness comes from the supervisor session")
	}
	// The policy JSON after "v1:" must parse (the gateway compares it parsed).
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(DefaultAdmissionPolicy, "v1:")), &v); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*extpb.PeerMetadata{nil, gatewayMeta(2)} {
		if _, err := d.GetCapabilities(context.Background(), &pb.GetCapabilitiesRequest{Gateway: bad}); err == nil {
			t.Errorf("gateway %v must be rejected", bad)
		}
	}
}

func TestBlaxelNameIsStableAndDNSSafe(t *testing.T) {
	a := blaxelName("My Sandbox_With.Weird Chars and a very long name", "id-1")
	if a != blaxelName("My Sandbox_With.Weird Chars and a very long name", "id-1") || a == blaxelName("x", "id-2") {
		t.Fatal("name must be stable per id and differ across ids")
	}
	if !regexp.MustCompile(`^os-[a-z0-9-]{1,20}-[0-9a-f]{8}$`).MatchString(a) {
		t.Fatalf("unexpected name %q", a)
	}
}

func TestMemoryMiB(t *testing.T) {
	d := testDriver()
	for q, want := range map[string]int{"": 4096, "4Gi": 4096, "512Mi": 512, "1073741824": 1024} {
		sb := &pb.DriverSandbox{Spec: &pb.DriverSandboxSpec{Template: &pb.DriverSandboxTemplate{
			Resources: &pb.DriverResourceRequirements{MemoryLimit: q}}}}
		if got, err := d.memoryMiB(sb); err != nil || got != want {
			t.Errorf("memoryMiB(%q) = %d, %v; want %d", q, got, err, want)
		}
	}
}

func TestSpecFromRequest(t *testing.T) {
	sb := &pb.DriverSandbox{Spec: &pb.DriverSandboxSpec{
		Command: []string{"claude"}, Tty: true, AwaitMainProcessAttachment: true,
		Environment: map[string]string{"FOO": "bar", "OPENSHELL_ENDPOINT": "https://attacker", "BAD NAME": "x"},
	}}
	s := specFromRequest(sb, "blaxel/py-app:latest", "info")
	if s.ChildEnv["FOO"] != "bar" || s.ChildEnv["OPENSHELL_ENDPOINT"] != "" || s.ChildEnv["BAD NAME"] != "" {
		t.Fatalf("child env = %v", s.ChildEnv)
	}
	var mp map[string]any
	json.Unmarshal(s.MainProcessSpec, &mp)
	if mp["version"].(float64) != 1 || mp["tty"] != true || mp["await_main_process_attachment"] != true || mp["command"].([]any)[0] != "claude" {
		t.Fatalf("main process spec = %s", s.MainProcessSpec)
	}
	scratch := specFromRequest(&pb.DriverSandbox{Spec: &pb.DriverSandboxSpec{}}, "img", "")
	if !strings.Contains(string(scratch.MainProcessSpec), `"command":[]`) || scratch.LogLevel != "" {
		t.Fatalf("scratch spec = %s", scratch.MainProcessSpec)
	}
}

func TestSupervisorEnv(t *testing.T) {
	d := testDriver()
	env := strings.Join(supervisorEnv(d.cfg, &record{id: "sb-1", name: "demo"}, "/state/sb-1/gen", &persistedSpec{
		MainProcessSpec: json.RawMessage(`{"version":1}`), LogLevel: "info"}), "\n")
	for _, want := range []string{
		"OPENSHELL_ADMITTED_ISOLATION_BACKEND=openshell-sandbox", "OPENSHELL_SANDBOX_ID=sb-1",
		"OPENSHELL_ENDPOINT=https://127.0.0.1:17670", "OPENSHELL_TLS_CA=/ca", "OPENSHELL_TLS_CERT=/crt", "OPENSHELL_TLS_KEY=/key",
		`OPENSHELL_MAIN_PROCESS_SPEC={"version":1}`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("supervisor env missing %s", want)
		}
	}
	if p := sshSocketPath(&record{id: "0b6f1d7e-9d1c-4a3e-9a52-7c2a3c1f5e10"}); len(p) >= 108 {
		t.Errorf("ssh socket path %q exceeds SUN_LEN", p)
	}
	if strings.Contains(env, "BL_API_KEY") {
		t.Error("the supervisor must not inherit Blaxel credentials")
	}
}

func TestLaunchScriptFence(t *testing.T) {
	s := launchScript()
	for _, want := range []string{"unshare --mount --net --pid --fork --kill-child", "ip link set lo up",
		"ip_unprivileged_port_start", "launch-capability-free 1500 1500 " + vmBootstrap + " /sandbox", "env -i"} {
		if !strings.Contains(s, want) {
			t.Errorf("launcher missing %q", want)
		}
	}
}

func TestOwnershipScopesRecovery(t *testing.T) {
	d := testDriver()
	if !d.owns(map[string]string{}) || d.owns(map[string]string{labelOwner: "other"}) {
		t.Error("default owner adopts unlabeled sandboxes only")
	}
	d.cfg.Owner = "other"
	if d.owns(map[string]string{}) || !d.owns(map[string]string{labelOwner: "other"}) {
		t.Error("a non-default owner adopts only its own sandboxes")
	}
}

func TestValidateWorkloadIdentity(t *testing.T) {
	d := testDriver()
	ok := &pb.DriverSandbox{Id: "a", Name: "b", Spec: &pb.DriverSandboxSpec{WorkloadIdentity: &pb.WorkloadIdentityRequest{User: "sandbox"}}}
	if _, err := d.ValidateSandboxCreate(context.Background(), &pb.ValidateSandboxCreateRequest{Sandbox: ok}); err != nil {
		t.Fatal(err)
	}
	bad := &pb.DriverSandbox{Id: "a", Name: "b", Spec: &pb.DriverSandboxSpec{WorkloadIdentity: &pb.WorkloadIdentityRequest{User: "root"}}}
	if _, err := d.ValidateSandboxCreate(context.Background(), &pb.ValidateSandboxCreateRequest{Sandbox: bad}); err == nil {
		t.Fatal("other identities must be rejected")
	}
}
