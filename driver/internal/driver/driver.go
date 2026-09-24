// Package driver implements the OpenShell v0.0.116 ComputeDriver contract on
// top of Blaxel sandboxes.
//
// Each OpenShell sandbox maps to one Blaxel sandbox (mk3 microVM). Inside it,
// openshell-sandbox runs as root with its in-VM supervisor (the v0.0.116
// model: netns + veth + policy proxy), and dials the gateway at
// https://127.0.0.1:<port>. That loopback port is the in-VM end of a reverse
// tunnel (see package tunnel) that this driver dials from the gateway host, so
// the sandbox never holds Blaxel credentials.
package driver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/blaxel-ai/openshell-blaxel/driver/gen/computev1"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/blaxel"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/tunnel"
)

const (
	DriverName    = "blaxel"
	DriverVersion = "0.1.0-poc"

	labelManaged   = "openshell.ai/managed-by"
	labelID        = "openshell.ai/sandbox-id"
	labelName      = "openshell.ai/sandbox-name"
	labelWorkspace = "openshell.ai/sandbox-workspace"
	labelNamespace = "openshell.ai/sandbox-namespace"

	sandboxProc = "openshell-sandbox"
	tunnelProc  = "os-tunnel"
	setupProc   = "os-setup"

	binDir        = "/opt/openshell/bin"
	launchScript  = "/opt/openshell/launch.sh"
	sshSocketPath = "/run/openshell/ssh.sock"
	tlsCAPath     = "/etc/openshell/tls/client/ca.crt"
	tlsCertPath   = "/etc/openshell/tls/client/tls.crt"
	tlsKeyPath    = "/etc/openshell/tls/client/tls.key"
	tokenPath     = "/etc/openshell/auth/sandbox.jwt"
)

type Config struct {
	Region       string
	DefaultImage string
	MemoryMiB    int
	// GatewayAddr is the gateway's host:port on the driver host.
	GatewayAddr string
	// InVMGatewayPort is the loopback port openshell-sandbox dials inside the VM.
	InVMGatewayPort int
	// TunnelPort is the Blaxel sandbox port the host WebSocket attaches to.
	TunnelPort int
	// TLSDir holds ca.crt and client/tls.{crt,key} for the sandbox's gateway mTLS.
	TLSDir string
	// Packages are installed as root at bootstrap; the workload runs
	// unprivileged and cannot install them itself.
	Packages []string
	// InstallClaude installs Claude Code at /usr/local/bin/claude, the path
	// OpenShell's claude-code provider profile grants network access to.
	InstallClaude bool
	// ExtraArgs is passed as spec.runtime.extraArgs to pick the kernel variant.
	ExtraArgs     map[string]string
	SandboxBinary string // linux x86_64 openshell-sandbox (v0.0.116)
	TunnelBinary  string // linux x86_64 os-tunnel
}

type Driver struct {
	pb.UnimplementedComputeDriverServer
	cfg Config
	bl  *blaxel.Client
	log *slog.Logger

	mu   sync.Mutex
	recs map[string]*record // by OpenShell sandbox id
	subs map[chan *pb.WatchSandboxesEvent]struct{}
}

type record struct {
	id, name, namespace, workspace string
	blxName, url                   string

	conds    []*pb.DriverCondition
	deleting bool
	stopped  bool
	ready    bool
	cancel   context.CancelFunc // tunnel + monitor for the current run
}

func New(cfg Config, bl *blaxel.Client, log *slog.Logger) *Driver {
	return &Driver{cfg: cfg, bl: bl, log: log, recs: map[string]*record{}, subs: map[chan *pb.WatchSandboxesEvent]struct{}{}}
}

// ---- snapshots and events ----

func (d *Driver) snapshotLocked(r *record) *pb.DriverSandbox {
	conds := make([]*pb.DriverCondition, len(r.conds))
	copy(conds, r.conds)
	return &pb.DriverSandbox{
		Id: r.id, Name: r.name, Namespace: r.namespace, Workspace: r.workspace,
		Status: &pb.DriverSandboxStatus{
			SandboxName: r.blxName,
			InstanceId:  r.blxName,
			Conditions:  conds,
			Deleting:    r.deleting,
		},
	}
}

func (d *Driver) broadcastLocked(ev *pb.WatchSandboxesEvent) {
	for ch := range d.subs {
		select {
		case ch <- ev:
		default: // slow watcher; the gateway re-lists on reconnect
		}
	}
}

// setStatus replaces a record's conditions and publishes the snapshot.
func (d *Driver) setStatus(r *record, conds ...*pb.DriverCondition) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r.conds = conds
	d.broadcastLocked(&pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_Sandbox{
		Sandbox: &pb.WatchSandboxesSandboxEvent{Sandbox: d.snapshotLocked(r)},
	}})
}

func ready(statusVal, reason, msg string) *pb.DriverCondition {
	return &pb.DriverCondition{Type: "Ready", Status: statusVal, Reason: reason, Message: msg,
		LastTransitionTime: time.Now().UTC().Format(time.RFC3339)}
}

func suspended() *pb.DriverCondition {
	return &pb.DriverCondition{Type: "Suspended", Status: "True", Reason: "Stopped",
		LastTransitionTime: time.Now().UTC().Format(time.RFC3339)}
}

func (d *Driver) event(r *record, typ, reason, msg string) {
	d.log.Info("platform event", "sandbox", r.name, "reason", reason, "msg", msg)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.broadcastLocked(&pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_PlatformEvent{
		PlatformEvent: &pb.WatchSandboxesPlatformEvent{SandboxId: r.id, Event: &pb.DriverPlatformEvent{
			TimestampMs: time.Now().UnixMilli(), Source: "blaxel", Type: typ, Reason: reason, Message: msg,
			Metadata: map[string]string{"blaxel_sandbox": r.blxName},
		}},
	}})
}

// ---- simple RPCs ----

func (d *Driver) GetCapabilities(context.Context, *pb.GetCapabilitiesRequest) (*pb.GetCapabilitiesResponse, error) {
	return &pb.GetCapabilitiesResponse{DriverName: DriverName, DriverVersion: DriverVersion, DefaultImage: d.cfg.DefaultImage}, nil
}

func (d *Driver) GetGatewayListenerRequirements(context.Context, *pb.GetGatewayListenerRequirementsRequest) (*pb.GetGatewayListenerRequirementsResponse, error) {
	// Sandboxes reach the gateway through the reverse tunnel, which dials the
	// gateway's primary listener on the host; no extra listener is needed.
	return &pb.GetGatewayListenerRequirementsResponse{}, nil
}

func (d *Driver) EnsureWorkspace(context.Context, *pb.EnsureWorkspaceRequest) (*pb.EnsureWorkspaceResponse, error) {
	return &pb.EnsureWorkspaceResponse{}, nil
}

func (d *Driver) DeleteWorkspace(context.Context, *pb.DeleteWorkspaceRequest) (*pb.DeleteWorkspaceResponse, error) {
	return &pb.DeleteWorkspaceResponse{}, nil
}

func (d *Driver) ValidateSandboxCreate(_ context.Context, req *pb.ValidateSandboxCreateRequest) (*pb.ValidateSandboxCreateResponse, error) {
	sb := req.GetSandbox()
	if sb.GetId() == "" || sb.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox id and name are required")
	}
	if sb.GetSpec().GetResourceRequirements().GetGpu() != nil {
		return nil, status.Error(codes.FailedPrecondition, "the Blaxel driver does not support GPU sandboxes")
	}
	if _, err := d.memoryMiB(sb); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.ValidateSandboxCreateResponse{}, nil
}

func (d *Driver) GetSandbox(_ context.Context, req *pb.GetSandboxRequest) (*pb.GetSandboxResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.find(req.GetSandboxId(), req.GetSandboxName())
	if r == nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", req.GetSandboxId())
	}
	return &pb.GetSandboxResponse{Sandbox: d.snapshotLocked(r)}, nil
}

func (d *Driver) ListSandboxes(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*pb.DriverSandbox, 0, len(d.recs))
	for _, r := range d.recs {
		out = append(out, d.snapshotLocked(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return &pb.ListSandboxesResponse{Sandboxes: out}, nil
}

func (d *Driver) WatchSandboxes(_ *pb.WatchSandboxesRequest, stream pb.ComputeDriver_WatchSandboxesServer) error {
	ch := make(chan *pb.WatchSandboxesEvent, 256)
	d.mu.Lock()
	for _, r := range d.recs {
		ch <- &pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_Sandbox{
			Sandbox: &pb.WatchSandboxesSandboxEvent{Sandbox: d.snapshotLocked(r)}}}
	}
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.subs, ch)
		d.mu.Unlock()
	}()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev := <-ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (d *Driver) find(id, name string) *record {
	if r, ok := d.recs[id]; ok {
		return r
	}
	if name != "" {
		for _, r := range d.recs {
			if r.name == name || r.blxName == name {
				return r
			}
		}
	}
	return nil
}

// ---- lifecycle RPCs ----

func (d *Driver) CreateSandbox(_ context.Context, req *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	sb := req.GetSandbox()
	d.mu.Lock()
	if r := d.recs[sb.GetId()]; r != nil {
		d.mu.Unlock()
		return &pb.CreateSandboxResponse{}, nil // idempotent retry
	}
	r := &record{id: sb.GetId(), name: sb.GetName(), namespace: sb.GetNamespace(), workspace: sb.GetWorkspace(),
		blxName: blaxelName(sb.GetName(), sb.GetId())}
	d.recs[r.id] = r
	d.mu.Unlock()

	d.setStatus(r, ready("False", "Starting", "creating Blaxel sandbox "+r.blxName))
	// Provisioning takes tens of seconds; the gateway follows progress on Watch.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := d.provision(ctx, r, sb); err != nil {
			d.log.Error("provision failed", "sandbox", r.name, "err", err)
			d.setStatus(r, ready("False", "ProvisionFailed", err.Error()))
		}
	}()
	return &pb.CreateSandboxResponse{}, nil
}

func (d *Driver) StopSandbox(ctx context.Context, req *pb.StopSandboxRequest) (*pb.StopSandboxResponse, error) {
	d.mu.Lock()
	r := d.find(req.GetSandboxId(), req.GetSandboxName())
	if r == nil {
		d.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", req.GetSandboxId())
	}
	r.stopped, r.ready = true, false
	cancel := r.cancel
	r.cancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if r.url != "" {
		if err := d.bl.KillProcess(ctx, r.url, sandboxProc); err != nil && !errors.Is(err, blaxel.ErrNotFound) {
			d.log.Warn("kill sandbox process", "sandbox", r.name, "err", err)
		}
	}
	// With no tunnel attached the VM idles into Blaxel standby.
	d.setStatus(r, ready("False", "ContainerStopped", "stopped"), suspended())
	return &pb.StopSandboxResponse{}, nil
}

func (d *Driver) StartSandbox(_ context.Context, req *pb.StartSandboxRequest) (*pb.StartSandboxResponse, error) {
	d.mu.Lock()
	r := d.find(req.GetSandboxId(), req.GetSandboxName())
	if r == nil {
		d.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", req.GetSandboxId())
	}
	if !r.stopped && r.cancel != nil {
		d.mu.Unlock()
		return &pb.StartSandboxResponse{}, nil
	}
	r.stopped = false
	d.mu.Unlock()
	d.setStatus(r, ready("False", "Starting", "starting"))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := d.run(ctx, r); err != nil {
			d.setStatus(r, ready("False", "StartFailed", err.Error()))
		}
	}()
	return &pb.StartSandboxResponse{}, nil
}

func (d *Driver) DeleteSandbox(ctx context.Context, req *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
	d.mu.Lock()
	r := d.find(req.GetSandboxId(), req.GetSandboxName())
	if r == nil {
		d.mu.Unlock()
		return &pb.DeleteSandboxResponse{Deleted: false}, nil
	}
	r.deleting = true
	cancel := r.cancel
	r.cancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	d.setStatus(r, ready("False", "Deleting", "deleting Blaxel sandbox"))
	err := d.bl.DeleteSandbox(ctx, r.blxName)
	if err != nil && !errors.Is(err, blaxel.ErrNotFound) {
		return nil, status.Errorf(codes.Unavailable, "delete %s: %v", r.blxName, err)
	}
	d.mu.Lock()
	delete(d.recs, r.id)
	d.broadcastLocked(&pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_Deleted{
		Deleted: &pb.WatchSandboxesDeletedEvent{SandboxId: r.id}}})
	d.mu.Unlock()
	return &pb.DeleteSandboxResponse{Deleted: err == nil}, nil
}

// ---- provisioning ----

func (d *Driver) provision(ctx context.Context, r *record, sb *pb.DriverSandbox) error {
	image := d.image(r, sb)
	mem, _ := d.memoryMiB(sb)
	labels := map[string]string{labelManaged: "openshell-driver-blaxel", labelID: sanitizeLabel(r.id),
		labelName: sanitizeLabel(r.name), labelWorkspace: sanitizeLabel(r.workspace), labelNamespace: sanitizeLabel(r.namespace)}
	d.event(r, "Normal", "Creating", fmt.Sprintf("image=%s memory=%dMiB region=%s extraArgs=%v", image, mem, d.cfg.Region, d.cfg.ExtraArgs))
	created, err := d.bl.CreateSandbox(ctx, blaxel.Sandbox{
		Metadata: blaxel.Metadata{Name: r.blxName, Labels: labels},
		Spec: blaxel.Spec{Region: d.cfg.Region, Runtime: blaxel.Runtime{
			Image: image, Memory: mem, Generation: "mk3", ExtraArgs: d.cfg.ExtraArgs,
			Ports: []blaxel.Port{{Target: d.cfg.TunnelPort, Protocol: "HTTP"}},
		}},
	})
	if err != nil {
		return fmt.Errorf("create Blaxel sandbox: %w", err)
	}
	url, err := d.waitDeployed(ctx, r.blxName, created)
	if err != nil {
		return err
	}
	d.mu.Lock()
	r.url = url
	d.mu.Unlock()
	d.event(r, "Normal", "Deployed", url)

	if err := d.bootstrap(ctx, r, sb, image); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return d.run(ctx, r)
}

func (d *Driver) waitDeployed(ctx context.Context, name string, sb *blaxel.Sandbox) (string, error) {
	for {
		if sb != nil {
			switch strings.ToUpper(sb.Status) {
			case "DEPLOYED":
				if sb.Metadata.URL != "" {
					return sb.Metadata.URL, nil
				}
			case "FAILED", "TERMINATED":
				return "", fmt.Errorf("Blaxel sandbox %s is %s", name, sb.Status)
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for %s to deploy: %w", name, ctx.Err())
		case <-time.After(2 * time.Second):
		}
		var err error
		if sb, err = d.bl.GetSandbox(ctx, name); err != nil {
			d.log.Warn("get sandbox", "name", name, "err", err)
			sb = nil
		}
	}
}

// bootstrap installs the runtime, credentials and launch script. It runs once
// per Blaxel sandbox; Start and driver restarts reuse what it wrote.
func (d *Driver) bootstrap(ctx context.Context, r *record, sb *pb.DriverSandbox, image string) error {
	files := []struct{ src, dst, perm string }{
		{d.cfg.SandboxBinary, binDir + "/openshell-sandbox", "0755"},
		{d.cfg.TunnelBinary, binDir + "/os-tunnel", "0755"},
		{filepath.Join(d.cfg.TLSDir, "ca.crt"), tlsCAPath, "0644"},
		{filepath.Join(d.cfg.TLSDir, "client", "tls.crt"), tlsCertPath, "0644"},
		{filepath.Join(d.cfg.TLSDir, "client", "tls.key"), tlsKeyPath, "0600"},
	}
	d.event(r, "Normal", "Uploading", "openshell-sandbox, os-tunnel, gateway client TLS")
	for _, f := range files {
		data, err := os.ReadFile(f.src)
		if err != nil {
			return err
		}
		if err := d.bl.WriteFile(ctx, r.url, f.dst, data, f.perm); err != nil {
			return fmt.Errorf("upload %s: %w", f.dst, err)
		}
	}
	spec := sb.GetSpec()
	if tok := spec.GetSandboxToken(); tok != "" {
		if err := d.bl.WriteFile(ctx, r.url, tokenPath, []byte(tok), "0600"); err != nil {
			return fmt.Errorf("upload token: %w", err)
		}
	}
	if err := d.bl.WriteFile(ctx, r.url, launchScript, []byte(d.launchScript(r, sb, image)), "0700"); err != nil {
		return fmt.Errorf("upload launch script: %w", err)
	}

	// iproute2/nftables are required by the v0.0.116 in-VM supervisor; a
	// prebuilt Blaxel template would remove this step.
	pkgs := strings.Join(append([]string{"iproute2", "nftables", "iptables"}, d.cfg.Packages...), " ")
	d.event(r, "Normal", "Installing", pkgs+", sandbox user")
	// The filesystem API ignores the multipart permissions field, so modes are
	// set here.
	setup := `set -e
chmod 755 ` + binDir + `/openshell-sandbox ` + binDir + `/os-tunnel
chmod 600 ` + tlsKeyPath + ` ` + tokenPath + ` 2>/dev/null || chmod 600 ` + tlsKeyPath + `
chmod 700 ` + launchScript + `
if command -v apt-get >/dev/null; then
  apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends ` + pkgs + ` >/dev/null
elif command -v apk >/dev/null; then
  apk add -q ` + pkgs + `
fi
id sandbox >/dev/null 2>&1 || useradd -m -u 1500 -s /bin/sh sandbox 2>/dev/null || adduser -D -u 1500 sandbox
mkdir -p /sandbox /run/openshell && chown sandbox:sandbox /sandbox
` + d.claudeSetup() + `
echo setup-ok`
	return d.runLong(ctx, r.url, setupProc, setup, 5*time.Minute)
}

// claudeSetup installs the native Claude Code binary as a real file (not a
// symlink) so the policy's binary identity matches /usr/local/bin/claude.
func (d *Driver) claudeSetup() string {
	if !d.cfg.InstallClaude {
		return ""
	}
	// Blaxel's process API runs with HOME=/blaxel; pin it so the installer's
	// location is known.
	return `if [ ! -x /usr/local/bin/claude ]; then
  export HOME=/root
  curl -fsSL https://claude.ai/install.sh | bash >/tmp/claude-install.log 2>&1 || { tail -20 /tmp/claude-install.log; exit 1; }
  install -m 0755 "$(readlink -f /root/.local/bin/claude)" /usr/local/bin/claude
fi
/usr/local/bin/claude --version`
}

// launchScript renders the root-owned script that starts openshell-sandbox
// with the environment the v0.0.116 Podman driver provides.
func (d *Driver) launchScript(r *record, sb *pb.DriverSandbox, image string) string {
	spec := sb.GetSpec()
	userEnv := map[string]string{}
	for k, v := range spec.GetTemplate().GetEnvironment() {
		userEnv[k] = v
	}
	for k, v := range spec.GetEnvironment() {
		userEnv[k] = v
	}
	env := map[string]string{}
	for k, v := range userEnv {
		if !strings.HasPrefix(k, "OPENSHELL_") { // driver-owned names cannot be overridden
			env[k] = v
		}
	}
	if len(userEnv) > 0 {
		b, _ := json.Marshal(userEnv)
		env["OPENSHELL_USER_ENVIRONMENT"] = string(b)
	}
	command := spec.GetCommand()
	tty := spec.GetTty()
	if len(command) == 0 {
		command, tty = []string{"/bin/bash", "-l"}, true
	}
	mainSpec, _ := json.Marshal(struct {
		Version int      `json:"version"`
		Command []string `json:"command"`
		TTY     bool     `json:"tty"`
	}{1, command, tty})

	logLevel := spec.GetLogLevel()
	if logLevel == "" {
		logLevel = "info"
	}
	for k, v := range map[string]string{
		"OPENSHELL_SANDBOX":                      r.name,
		"OPENSHELL_SANDBOX_ID":                   r.id,
		"OPENSHELL_ENDPOINT":                     fmt.Sprintf("https://127.0.0.1:%d", d.cfg.InVMGatewayPort),
		"OPENSHELL_SSH_SOCKET_PATH":              sshSocketPath,
		"OPENSHELL_CONTAINER_IMAGE":              image,
		"OPENSHELL_MAIN_PROCESS_SPEC":            "base64url:" + base64.RawURLEncoding.EncodeToString(mainSpec),
		"OPENSHELL_TELEMETRY_ENABLED":            "false",
		"OPENSHELL_NETWORK_RUNTIME_CAPABILITIES": "policy-dns-transparent-tcp",
		"OPENSHELL_TLS_CA":                       tlsCAPath,
		"OPENSHELL_TLS_CERT":                     tlsCertPath,
		"OPENSHELL_TLS_KEY":                      tlsKeyPath,
		"OPENSHELL_SANDBOX_TOKEN_FILE":           tokenPath,
		// Blaxel images declare no USER; run the workload as the user bootstrap creates.
		"OPENSHELL_OCI_IMAGE_USER": "sandbox",
		"OPENSHELL_SANDBOX_UID":    "",
		"OPENSHELL_SANDBOX_GID":    "",
		"OPENSHELL_LOG_LEVEL":      logLevel,
	} {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if validEnvName.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Generated by openshell-driver-blaxel. Starts the OpenShell sandbox supervisor.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(env[k]))
	}
	b.WriteString("cd /sandbox\nexec " + binDir + "/openshell-sandbox --workdir /sandbox\n")
	return b.String()
}

var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// run (re)starts the tunnel and the sandbox process, then monitors them.
func (d *Driver) run(ctx context.Context, r *record) error { return d.start(ctx, r, true) }

// reattach reconnects the tunnel to a sandbox whose supervisor is still
// running (after a driver restart) without restarting the workload; the
// in-VM supervisor reconnects to the gateway on its own.
func (d *Driver) reattach(ctx context.Context, r *record) error { return d.start(ctx, r, false) }

func (d *Driver) start(ctx context.Context, r *record, restartSandbox bool) error {
	tunnelRunning := false
	if p, err := d.bl.GetProcess(ctx, r.url, tunnelProc); err == nil && p.Status == "running" {
		tunnelRunning = true
	}
	if restartSandbox || !tunnelRunning {
		// A fresh endpoint drops any session left from a previous run.
		_ = d.bl.KillProcess(ctx, r.url, tunnelProc)
		if _, err := d.bl.Exec(ctx, r.url, blaxel.ProcessRequest{
			Name: tunnelProc, KeepAlive: true, Timeout: blaxel.Forever,
			Command: fmt.Sprintf("%s/os-tunnel -ws :%d -local 127.0.0.1:%d", binDir, d.cfg.TunnelPort, d.cfg.InVMGatewayPort),
		}); err != nil {
			return fmt.Errorf("start tunnel endpoint: %w", err)
		}
	}

	runCtx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.cancel = cancel
	d.mu.Unlock()

	up := make(chan struct{}, 1)
	wsURL := strings.Replace(r.url, "https://", "wss://", 1) + fmt.Sprintf("/port/%d/tunnel", d.cfg.TunnelPort)
	go tunnel.Dial(runCtx, wsURL, d.bl.Headers, d.cfg.GatewayAddr, d.log.With("sandbox", r.name), func(ok bool) {
		if ok {
			select {
			case up <- struct{}{}:
			default:
			}
		}
	})
	select {
	case <-up:
		d.event(r, "Normal", "TunnelUp", "gateway reachable from the VM at 127.0.0.1:"+strconv.Itoa(d.cfg.InVMGatewayPort))
	case <-time.After(60 * time.Second):
		cancel()
		return errors.New("reverse tunnel did not come up within 60s")
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}

	if restartSandbox {
		_ = d.bl.KillProcess(ctx, r.url, sandboxProc)
		if _, err := d.bl.Exec(ctx, r.url, blaxel.ProcessRequest{Name: sandboxProc, Command: "sh " + launchScript, KeepAlive: true, Timeout: blaxel.Forever}); err != nil {
			cancel()
			return fmt.Errorf("start openshell-sandbox: %w", err)
		}
		d.event(r, "Normal", "Started", "openshell-sandbox launched")
	}
	go d.monitor(runCtx, r)
	return nil
}

// monitor reports Ready once the in-VM supervisor has created its SSH socket
// (the readiness signal the Podman driver's healthcheck uses) and reports
// exits afterwards.
func (d *Driver) monitor(ctx context.Context, r *record) {
	interval := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		p, err := d.bl.GetProcess(ctx, r.url, sandboxProc)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.log.Warn("get process", "sandbox", r.name, "err", err)
			continue
		}
		if p.Status != "running" {
			d.mu.Lock()
			stopped := r.stopped
			r.ready = false
			d.mu.Unlock()
			if !stopped {
				d.setStatus(r, ready("False", "ContainerExited",
					fmt.Sprintf("openshell-sandbox %s (exit %d): %s", p.Status, p.ExitCode, tail(p.Logs, 800))))
			}
			return
		}
		d.mu.Lock()
		isReady := r.ready
		d.mu.Unlock()
		if isReady {
			interval = 10 * time.Second
			continue
		}
		if _, err := d.bl.Run(ctx, r.url, "test -S "+sshSocketPath); err == nil {
			d.mu.Lock()
			r.ready = true
			d.mu.Unlock()
			d.setStatus(r, ready("True", "HealthCheckPassed", ""))
		}
	}
}

// runLong runs a command that may outlast one sandbox-API request.
func (d *Driver) runLong(ctx context.Context, url, name, script string, timeout time.Duration) error {
	_ = d.bl.KillProcess(ctx, url, name)
	if _, err := d.bl.Exec(ctx, url, blaxel.ProcessRequest{Name: name, Command: "sh -c " + shellQuote(script)}); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		p, err := d.bl.GetProcess(ctx, url, name)
		if err != nil {
			continue
		}
		switch p.Status {
		case "running":
			continue
		case "completed":
			if p.ExitCode == 0 {
				return nil
			}
		}
		return fmt.Errorf("%s %s (exit %d): %s", name, p.Status, p.ExitCode, tail(p.Logs, 800))
	}
	return fmt.Errorf("%s timed out after %s", name, timeout)
}

// ---- recovery ----

// Recover rebuilds state from Blaxel after a driver restart and reattaches
// tunnels to sandboxes whose supervisor is still running.
func (d *Driver) Recover(ctx context.Context) error {
	all, err := d.bl.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	for _, s := range all {
		l := s.Metadata.Labels
		if l[labelManaged] != "openshell-driver-blaxel" || l[labelID] == "" {
			continue
		}
		r := &record{id: l[labelID], name: l[labelName], workspace: l[labelWorkspace], namespace: l[labelNamespace],
			blxName: s.Metadata.Name, url: s.Metadata.URL}
		d.mu.Lock()
		d.recs[r.id] = r
		d.mu.Unlock()
		p, err := d.bl.GetProcess(ctx, r.url, sandboxProc)
		if err == nil && p.Status == "running" {
			d.log.Info("recovered running sandbox", "sandbox", r.name)
			d.setStatus(r, ready("False", "Starting", "reattaching after driver restart"))
			go func() {
				if err := d.reattach(context.Background(), r); err != nil {
					d.setStatus(r, ready("False", "StartFailed", err.Error()))
				}
			}()
		} else {
			d.mu.Lock()
			r.stopped = true
			d.mu.Unlock()
			d.setStatus(r, ready("False", "ContainerStopped", "not running"), suspended())
		}
	}
	return nil
}

// Close stops all tunnels (sandboxes keep running in Blaxel).
func (d *Driver) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.recs {
		if r.cancel != nil {
			r.cancel()
		}
	}
}

// ---- helpers ----

func (d *Driver) image(r *record, sb *pb.DriverSandbox) string {
	img := sb.GetSpec().GetTemplate().GetImage()
	if img == "" {
		return d.cfg.DefaultImage
	}
	// Blaxel images must embed Blaxel's sandbox-api; arbitrary OCI images
	// (such as OpenShell's community base image) do not.
	if strings.HasPrefix(img, "blaxel/") || strings.HasPrefix(img, "sandbox/") {
		return img
	}
	d.event(r, "Warning", "ImageSubstituted",
		fmt.Sprintf("image %q is not a Blaxel sandbox image; using %s", img, d.cfg.DefaultImage))
	return d.cfg.DefaultImage
}

func (d *Driver) memoryMiB(sb *pb.DriverSandbox) (int, error) {
	q := sb.GetSpec().GetTemplate().GetResources().GetMemoryLimit()
	if q == "" {
		return d.cfg.MemoryMiB, nil
	}
	units := []struct {
		suffix string
		mib    float64
	}{{"Gi", 1024}, {"Mi", 1}, {"G", 1e9 / (1 << 20)}, {"M", 1e6 / (1 << 20)}}
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(q, u.suffix), 64)
			if err != nil {
				return 0, fmt.Errorf("invalid memory limit %q", q)
			}
			return int(v * u.mib), nil
		}
	}
	v, err := strconv.ParseInt(q, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory limit %q", q)
	}
	return int(v >> 20), nil
}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// blaxelName derives a stable, DNS-safe Blaxel sandbox name.
func blaxelName(name, id string) string {
	slug := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 20 {
		slug = strings.Trim(slug[:20], "-")
	}
	sum := sha256.Sum256([]byte(id))
	return "os-" + slug + "-" + hex.EncodeToString(sum[:4])
}

var nonLabel = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeLabel(s string) string {
	s = nonLabel.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
