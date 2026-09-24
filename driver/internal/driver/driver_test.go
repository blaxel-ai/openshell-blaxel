package driver

import (
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	pb "github.com/blaxel-ai/openshell-blaxel/driver/gen/computev1"
)

func testDriver() *Driver {
	return New(Config{DefaultImage: "blaxel/py-app:latest", MemoryMiB: 4096, InVMGatewayPort: 17680}, nil, nil)
}

func TestBlaxelNameIsStableAndDNSSafe(t *testing.T) {
	a := blaxelName("My Sandbox_With.Weird Chars and a very long name", "id-1")
	if a != blaxelName("My Sandbox_With.Weird Chars and a very long name", "id-1") {
		t.Fatal("name must be stable for the same id")
	}
	if a == blaxelName("My Sandbox_With.Weird Chars and a very long name", "id-2") {
		t.Fatal("different ids must give different names")
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
		got, err := d.memoryMiB(sb)
		if err != nil || got != want {
			t.Errorf("memoryMiB(%q) = %d, %v; want %d", q, got, err, want)
		}
	}
	bad := &pb.DriverSandbox{Spec: &pb.DriverSandboxSpec{Template: &pb.DriverSandboxTemplate{
		Resources: &pb.DriverResourceRequirements{MemoryLimit: "lots"}}}}
	if _, err := d.memoryMiB(bad); err == nil {
		t.Error("invalid quantity must fail")
	}
}

// runLaunchEnv executes the generated launch script with its final exec
// replaced by `env`, returning the environment the supervisor would get.
func runLaunchEnv(t *testing.T, script string) map[string]string {
	t.Helper()
	i := strings.LastIndex(script, "cd /sandbox\n")
	if i < 0 {
		t.Fatal("launch script lost its cd/exec tail")
	}
	out, err := exec.Command("/bin/sh", "-c", script[:i]+"env").Output()
	if err != nil {
		t.Fatalf("launch script failed to run: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

func TestLaunchScriptQuotingAndOwnership(t *testing.T) {
	d := testDriver()
	hostile := `$(touch /tmp/pwned); echo 'x'"y"` + "`id`"
	sb := &pb.DriverSandbox{Id: "sb-1", Name: "demo", Spec: &pb.DriverSandboxSpec{
		Command: []string{"claude", "--resume"},
		Environment: map[string]string{
			"FOO":                "bar baz",
			"HOSTILE":            hostile,
			"OPENSHELL_ENDPOINT": "https://attacker.example",
			"BAD NAME":           "dropped",
		},
	}}
	env := runLaunchEnv(t, d.launchScript(&record{id: "sb-1", name: "demo"}, sb, "blaxel/py-app:latest"))

	if env["FOO"] != "bar baz" || env["HOSTILE"] != hostile {
		t.Errorf("user values must arrive verbatim: FOO=%q HOSTILE=%q", env["FOO"], env["HOSTILE"])
	}
	if env["OPENSHELL_ENDPOINT"] != "https://127.0.0.1:17680" {
		t.Errorf("user env must not override driver-owned names, got %q", env["OPENSHELL_ENDPOINT"])
	}
	if _, ok := env["BAD NAME"]; ok {
		t.Error("invalid variable names must be dropped")
	}
	if env["OPENSHELL_OCI_IMAGE_USER"] != "sandbox" || env["OPENSHELL_SANDBOX_TOKEN_FILE"] != tokenPath {
		t.Errorf("missing driver env: %v", env)
	}

	spec, ok := strings.CutPrefix(env["OPENSHELL_MAIN_PROCESS_SPEC"], "base64url:")
	if !ok {
		t.Fatalf("main process spec not base64url-encoded: %q", env["OPENSHELL_MAIN_PROCESS_SPEC"])
	}
	raw, err := base64.RawURLEncoding.DecodeString(spec)
	if err != nil {
		t.Fatal(err)
	}
	var mp struct {
		Version int      `json:"version"`
		Command []string `json:"command"`
	}
	if err := json.Unmarshal(raw, &mp); err != nil || mp.Version != 1 || strings.Join(mp.Command, " ") != "claude --resume" {
		t.Errorf("main process spec = %s (%v)", raw, err)
	}
}

func TestImageSubstitution(t *testing.T) {
	d := testDriver()
	d.subs = map[chan *pb.WatchSandboxesEvent]struct{}{}
	d.log = discardLogger()
	for img, want := range map[string]string{
		"":                              "blaxel/py-app:latest",
		"blaxel/node:latest":            "blaxel/node:latest",
		"sandbox/my-template:abc":       "sandbox/my-template:abc",
		"ghcr.io/nvidia/openshell/base": "blaxel/py-app:latest",
	} {
		sb := &pb.DriverSandbox{Spec: &pb.DriverSandboxSpec{Template: &pb.DriverSandboxTemplate{Image: img}}}
		if got := d.image(&record{}, sb); got != want {
			t.Errorf("image(%q) = %q, want %q", img, got, want)
		}
	}
}
