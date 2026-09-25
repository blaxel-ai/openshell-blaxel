package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "github.com/blaxel-ai/openshell-blaxel/driver/gen/computev1"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/blaxel"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/boundary"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/tunnel"
)

const (
	workloadUser = "sandbox"
	workloadUID  = 1500

	// In the workload VM.
	vmBinDir      = "/opt/openshell/bin"
	vmLaunch      = "/opt/openshell/launch.sh"
	vmStateDir    = "/.openshell/state"
	vmBootstrap   = vmStateDir + "/bootstrap.json"
	vmTLSCert     = vmStateDir + "/sandbox.crt"
	vmTLSKey      = vmStateDir + "/sandbox.key"
	vmBoundaryDir = "/run/openshell-boundary"
	vmSocket      = vmBoundaryDir + "/control.sock"

	sandboxProc = "openshell-sandbox"
	tunnelProc  = "os-tunnel"
	setupProc   = "os-setup"
)

func validGeneration(g string) bool { return boundary.ValidGeneration(g) }

// runtime is one generation's control-side state.
type runtime struct {
	cancel     context.CancelFunc
	supervisor *exec.Cmd
	liveness   *os.File // write end; closing it makes the supervisor exit
	exited     chan struct{}
}

// persistedSpec keeps what StartSandbox needs, which the gateway does not
// resend: the canonical process and workload environment.
type persistedSpec struct {
	Image           string            `json:"image"`
	ChildEnv        map[string]string `json:"child_env"`
	MainProcessSpec json.RawMessage   `json:"main_process_spec"`
	LogLevel        string            `json:"log_level"`
}

func (d *Driver) sandboxDir(r *record) string { return filepath.Join(d.cfg.StateDir, r.id) }

func (d *Driver) removeState(r *record) { os.RemoveAll(d.sandboxDir(r)) }

func specFromRequest(sb *pb.DriverSandbox, image, defaultLog string) persistedSpec {
	spec := sb.GetSpec()
	env := map[string]string{}
	for k, v := range spec.GetTemplate().GetEnvironment() {
		env[k] = v
	}
	for k, v := range spec.GetEnvironment() {
		env[k] = v
	}
	child := map[string]string{}
	for k, v := range env {
		if validEnvName.MatchString(k) && !strings.HasPrefix(k, "OPENSHELL_") {
			child[k] = v
		}
	}
	cmd, tty := spec.GetCommand(), spec.GetTty()
	if len(cmd) == 0 {
		cmd, tty = []string{}, true // scratch: the supervisor's default shell
	}
	mp, _ := json.Marshal(struct {
		Version  int      `json:"version"`
		Command  []string `json:"command"`
		TTY      bool     `json:"tty"`
		AwaitAtt bool     `json:"await_main_process_attachment"`
	}{1, cmd, tty, spec.GetAwaitMainProcessAttachment()})
	lvl := spec.GetLogLevel()
	if lvl == "" {
		lvl = defaultLog
	}
	return persistedSpec{Image: image, ChildEnv: child, MainProcessSpec: mp, LogLevel: lvl}
}

func (d *Driver) saveSpec(r *record, s persistedSpec) error {
	if err := os.MkdirAll(d.sandboxDir(r), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return os.WriteFile(filepath.Join(d.sandboxDir(r), "spec.json"), b, 0o600)
}

func (d *Driver) loadSpec(r *record) (persistedSpec, error) {
	var s persistedSpec
	b, err := os.ReadFile(filepath.Join(d.sandboxDir(r), "spec.json"))
	if err != nil {
		return s, fmt.Errorf("sandbox spec not found on the control plane: %w", err)
	}
	return s, json.Unmarshal(b, &s)
}

// ---- provisioning (once per Blaxel sandbox) ----

func (d *Driver) provision(ctx context.Context, r *record, sb *pb.DriverSandbox) error {
	image := d.image(r, sb)
	mem, _ := d.memoryMiB(sb)
	spec := specFromRequest(sb, image, d.cfg.LogLevel)
	if err := d.saveSpec(r, spec); err != nil {
		return err
	}
	labels := map[string]string{labelManaged: "openshell-driver-blaxel", labelID: sanitizeLabel(r.id),
		labelName: sanitizeLabel(r.name), labelWorkspace: sanitizeLabel(r.workspace), labelNamespace: sanitizeLabel(r.namespace),
		labelOwner: sanitizeLabel(d.cfg.Owner)}
	d.event(r, "Normal", "Creating", fmt.Sprintf("image=%s memory=%dMiB region=%s extraArgs=%v", image, mem, d.cfg.Region, d.cfg.ExtraArgs))
	created, err := d.bl.CreateSandbox(ctx, blaxel.SandboxSpec{
		Name: r.blxName, Labels: labels, Region: d.cfg.Region, Image: image, MemoryMiB: mem,
		ExtraArgs: d.cfg.ExtraArgs, Ports: []blaxel.Port{{Target: d.cfg.TunnelPort, Protocol: "HTTP"}},
	})
	if err != nil {
		return fmt.Errorf("create Blaxel sandbox: %w", err)
	}
	url, err := d.waitDeployed(ctx, r.blxName, created)
	if err != nil {
		return err
	}
	d.mu.Lock()
	r.url, r.image = url, image
	d.mu.Unlock()
	d.event(r, "Normal", "Deployed", url)
	if err := d.waitSandboxAPI(ctx, r); err != nil {
		return err
	}
	if err := d.bootstrapVM(ctx, r); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return d.launch(ctx, r, sb.GetSpec().GetLaunchAuthentication(), &spec)
}

func (d *Driver) waitDeployed(ctx context.Context, name string, sb *blaxel.Sandbox) (string, error) {
	for {
		if sb != nil {
			switch strings.ToUpper(sb.Status) {
			case "DEPLOYED":
				if sb.URL != "" {
					return sb.URL, nil
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
			sb = nil
		}
	}
}

// waitSandboxAPI waits until the VM answers: DEPLOYED can precede boot, and a
// kernel that fails to boot never answers (see docs/KERNEL.md).
func (d *Driver) waitSandboxAPI(ctx context.Context, r *record) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := d.bl.Run(ctx, r.blxName, "true"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the workload VM did not answer within 90s (kernel variant failing to boot?)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// launchScript starts openshell-sandbox capability-free as PID 1 of fresh
// mount, network and PID namespaces. The network namespace only has loopback:
// that is the outer fence the driver asserts (every guarantee in
// boundary.OuterFence). The Unix listener lives on the shared filesystem, so
// os-tunnel in the root namespace can reach it.
func launchScript() string {
	return `#!/bin/sh
# Generated by openshell-driver-blaxel. Starts openshell-sandbox (OpenShell main)
# capability-free inside mount/network/PID namespaces whose only interface is lo.
set -e
chown -R ` + strconv.Itoa(workloadUID) + `:` + strconv.Itoa(workloadUID) + ` ` + vmStateDir + ` ` + vmBoundaryDir + ` /sandbox /run/openshell-supervisor-ca
chmod 700 ` + vmStateDir + ` ` + vmBoundaryDir + `
chmod 600 ` + vmStateDir + `/*
rm -f ` + vmSocket + `
exec env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/sandbox USER=` + workloadUser + ` LANG=C.UTF-8 OPENSHELL_LOG_LEVEL="${1:-info}" \
  unshare --mount --net --pid --fork --kill-child --mount-proc sh -c '
    ip link set lo up
    echo 0 > /proc/sys/net/ipv4/ip_unprivileged_port_start
    printf "nameserver 127.0.0.53\noptions timeout:2 attempts:2\n" > /run/openshell-resolv.conf
    mount --bind /run/openshell-resolv.conf /etc/resolv.conf
    exec ` + vmBinDir + `/openshell-sandbox launch-capability-free ` + strconv.Itoa(workloadUID) + ` ` + strconv.Itoa(workloadUID) + ` ` + vmBootstrap + ` /sandbox'
`
}

// bootstrapVM installs the runtime and prerequisites. It runs once per Blaxel
// sandbox; every generation reuses it.
func (d *Driver) bootstrapVM(ctx context.Context, r *record) error {
	d.event(r, "Normal", "Uploading", "openshell-sandbox, os-tunnel, launcher")
	for _, f := range []struct{ src, dst string }{
		{d.cfg.SandboxBinary, vmBinDir + "/openshell-sandbox"},
		{d.cfg.TunnelBinary, vmBinDir + "/os-tunnel"},
	} {
		data, err := os.ReadFile(f.src)
		if err != nil {
			return err
		}
		if err := d.bl.WriteFile(ctx, r.blxName, f.dst, data, "0755"); err != nil {
			return fmt.Errorf("upload %s: %w", f.dst, err)
		}
	}
	if err := d.bl.WriteFile(ctx, r.blxName, vmLaunch, []byte(launchScript()), "0700"); err != nil {
		return fmt.Errorf("upload launcher: %w", err)
	}
	// iproute2 (lo up) and util-linux (unshare) are the launcher's only
	// dependencies; the rest are workload conveniences.
	pkgs := strings.Join(append([]string{"iproute2", "util-linux", "ca-certificates"}, d.cfg.Packages...), " ")
	d.event(r, "Normal", "Installing", pkgs+", sandbox user")
	// The filesystem API ignores the multipart permissions field, so modes
	// are set here.
	setup := `set -e
chmod 755 ` + vmBinDir + `/openshell-sandbox ` + vmBinDir + `/os-tunnel
chmod 700 ` + vmLaunch + `
if command -v apt-get >/dev/null; then
  apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends ` + pkgs + ` >/dev/null
elif command -v apk >/dev/null; then
  apk add -q ` + pkgs + `
fi
id ` + workloadUser + ` >/dev/null 2>&1 || useradd -m -u ` + strconv.Itoa(workloadUID) + ` -s /bin/sh ` + workloadUser + ` 2>/dev/null || adduser -D -u ` + strconv.Itoa(workloadUID) + ` ` + workloadUser + `
mkdir -p ` + vmStateDir + ` ` + vmBoundaryDir + ` /sandbox /run/openshell-supervisor-ca
` + d.claudeSetup() + `
echo setup-ok`
	return d.runLong(ctx, r.blxName, setupProc, setup, 5*time.Minute)
}

// claudeSetup installs Claude Code as a real file at /usr/local/bin/claude,
// the path OpenShell's claude-code provider profile trusts.
func (d *Driver) claudeSetup() string {
	if !d.cfg.InstallClaude {
		return ""
	}
	// Blaxel's process API runs with HOME=/blaxel; pin it for the installer.
	return `if [ ! -x /usr/local/bin/claude ]; then
  export HOME=/root
  curl -fsSL https://claude.ai/install.sh | bash >/tmp/claude-install.log 2>&1 || { tail -20 /tmp/claude-install.log; exit 1; }
  install -m 0755 "$(readlink -f /root/.local/bin/claude)" /usr/local/bin/claude
fi
/usr/local/bin/claude --version`
}

// ---- one generation ----

// launch starts a generation from gateway launch credentials: fresh TLS
// identity, bootstrap and descriptor, the in-VM runtime, the tunnel and a
// supervisor. spec is nil on StartSandbox (read back from the control plane).
func (d *Driver) launch(ctx context.Context, r *record, rawAuth []byte, spec *persistedSpec) error {
	auth, err := boundary.DecodeLaunchAuth(rawAuth)
	if err != nil {
		return err
	}
	if spec == nil {
		s, err := d.loadSpec(r)
		if err != nil {
			return err
		}
		spec = &s
	}
	gen := auth.Session.RuntimeGeneration
	digest := sha256.Sum256([]byte(spec.Image))
	identity, err := boundary.NewWorkloadIdentity(workloadUID, workloadUID, "blaxel-image", "sha256:"+hex.EncodeToString(digest[:]))
	if err != nil {
		return err
	}
	ep, err := tunnel.Listen("127.0.0.1:0")
	if err != nil {
		return err
	}
	l, err := boundary.BuildLaunch(boundary.LaunchParams{
		SandboxID: r.id, Auth: auth, Identity: identity,
		ResourceClaims: map[string]string{"blaxel.sandbox": r.blxName, "blaxel.generation": gen},
		SocketPath:     vmSocket, CertPath: vmTLSCert, KeyPath: vmTLSKey,
		TransportAddr: ep.Addr().String(), ChildEnv: spec.ChildEnv,
	})
	if err != nil {
		return err
	}

	// Workload side: bootstrap and TLS server identity (consumed and deleted
	// by openshell-sandbox), then the runtime and the tunnel endpoint.
	cfgJSON, _ := json.Marshal(l.Config)
	for _, f := range []struct {
		path string
		data []byte
	}{{vmBootstrap, cfgJSON}, {vmTLSCert, []byte(l.TLS.CertificateChainPEM)}, {vmTLSKey, []byte(l.TLS.PrivateKeyPEM)}} {
		if err := d.bl.WriteFile(ctx, r.blxName, f.path, f.data, "0600"); err != nil {
			return fmt.Errorf("stage %s: %w", f.path, err)
		}
	}
	d.stopWorkload(ctx, r)
	if _, err := d.bl.Exec(ctx, r.blxName, blaxel.ProcessRequest{
		Name: sandboxProc, Command: "sh " + vmLaunch + " " + shellQuote(spec.LogLevel), KeepAlive: true, Timeout: blaxel.Forever,
	}); err != nil {
		return fmt.Errorf("start openshell-sandbox: %w", err)
	}
	if _, err := d.bl.Exec(ctx, r.blxName, blaxel.ProcessRequest{
		Name: tunnelProc, KeepAlive: true, Timeout: blaxel.Forever,
		Command: fmt.Sprintf("%s/os-tunnel serve -ws :%d -target unix:%s", vmBinDir, d.cfg.TunnelPort, vmSocket),
	}); err != nil {
		return fmt.Errorf("start tunnel endpoint: %w", err)
	}
	d.event(r, "Normal", "Started", fmt.Sprintf("openshell-sandbox generation %s", gen))

	runCtx, cancel := context.WithCancel(context.Background())
	wsURL := strings.Replace(r.url, "https://", "wss://", 1) + fmt.Sprintf("/port/%d/tunnel", d.cfg.TunnelPort)
	go ep.Run(runCtx, wsURL, d.bl.Headers, d.log.With("sandbox", r.name))
	select {
	case <-ep.Ready():
	case <-time.After(60 * time.Second):
		cancel()
		return errors.New("tunnel to the workload sandbox did not come up within 60s")
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
	d.event(r, "Normal", "TunnelUp", "supervisor transport "+ep.Addr().String())

	// Control side: descriptor + auth bundle, then the supervisor.
	rt, err := d.spawnSupervisor(r, l, spec)
	if err != nil {
		cancel()
		return err
	}
	rt.cancel = cancel
	d.mu.Lock()
	r.run = rt
	d.mu.Unlock()
	go d.watch(runCtx, r, rt)
	d.event(r, "Normal", "SupervisorStarted", "waiting for the supervisor session")
	return nil
}

func (d *Driver) spawnSupervisor(r *record, l *boundary.Launch, spec *persistedSpec) (*runtime, error) {
	dir := filepath.Join(d.sandboxDir(r), "gen-"+l.Config.Generation+"-"+strconv.FormatUint(l.Config.AuthEpoch, 10))
	if err := os.MkdirAll(filepath.Join(dir, "proxy-tls"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(sshSocketPath(r)), 0o700); err != nil {
		return nil, err
	}
	desc, _ := json.Marshal(l.Descriptor)
	descPath, authPath := filepath.Join(dir, "runtime-descriptor.json"), filepath.Join(dir, "supervisor-auth.json")
	if err := os.WriteFile(descPath, desc, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(authPath, l.SupervisorRaw, 0o600); err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdout, _ := os.OpenFile(filepath.Join(dir, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	stderr, _ := os.OpenFile(filepath.Join(dir, "supervisor.err.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)

	cmd := exec.Command(d.cfg.SupervisorBinary,
		"--backend-descriptor-file", descPath,
		"--auth-bundle-file", authPath,
		"--workdir", "/sandbox",
		"--main-exit-marker", filepath.Join(dir, "main-process-exited"),
		"--parent-liveness-fd", "3",
	)
	cmd.Env = supervisorEnv(d.cfg, r, dir, spec)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, stdout, stderr
	cmd.ExtraFiles = []*os.File{pr} // fd 3
	setParentDeathSignal(cmd)
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("start openshell-supervisor: %w", err)
	}
	pr.Close()
	rt := &runtime{supervisor: cmd, liveness: pw, exited: make(chan struct{})}
	go func() { cmd.Wait(); stdout.Close(); stderr.Close(); close(rt.exited) }()
	return rt, nil
}

// sshSocketPath is short on purpose: Unix socket paths are limited to 108
// bytes (SUN_LEN), too little for the per-generation state directory.
func sshSocketPath(r *record) string {
	sum := sha256.Sum256([]byte(r.id))
	return filepath.Join("/run/openshell-blaxel", hex.EncodeToString(sum[:6]), "ssh.sock")
}

// supervisorEnv mirrors the VM driver's host-supervisor environment, starting
// from an empty environment.
func supervisorEnv(cfg Config, r *record, dir string, spec *persistedSpec) []string {
	env := map[string]string{
		"OPENSHELL_ADMITTED_ISOLATION_BACKEND": "openshell-sandbox",
		"OPENSHELL_MAIN_PROCESS_SPEC":          string(spec.MainProcessSpec),
		"OPENSHELL_ENDPOINT":                   cfg.GatewayEndpoint,
		"OPENSHELL_SANDBOX_ID":                 r.id,
		"OPENSHELL_SANDBOX":                    r.name,
		"OPENSHELL_SSH_SOCKET_PATH":            sshSocketPath(r),
		"OPENSHELL_PROXY_TLS_DIR":              filepath.Join(dir, "proxy-tls"),
		"OPENSHELL_SANDBOX_UID":                strconv.Itoa(workloadUID),
		"OPENSHELL_SANDBOX_GID":                strconv.Itoa(workloadUID),
		"OPENSHELL_OCI_IMAGE_USER":             "",
		"OPENSHELL_LOG_LEVEL":                  spec.LogLevel,
		"OPENSHELL_TELEMETRY_ENABLED":          "false",
		"HOME":                                 dir,
	}
	if strings.HasPrefix(cfg.GatewayEndpoint, "https://") {
		env["OPENSHELL_TLS_CA"], env["OPENSHELL_TLS_CERT"], env["OPENSHELL_TLS_KEY"] = cfg.GatewayCA, cfg.GatewayCert, cfg.GatewayKey
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// watch reports the generation's end: supervisor exit on the control side, or
// openshell-sandbox exit in the workload VM.
func (d *Driver) watch(ctx context.Context, r *record, rt *runtime) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-rt.exited:
			if d.current(r, rt) {
				d.setStatus(r, failed("SupervisorExited", "openshell-supervisor exited: "+d.supervisorErr(r)))
			}
			return
		case <-tick.C:
			p, err := d.bl.GetProcess(ctx, r.blxName, sandboxProc)
			if ctx.Err() != nil || err != nil {
				continue
			}
			if p.Status != "running" && d.current(r, rt) {
				d.setStatus(r, failed("ProcessExited", fmt.Sprintf("openshell-sandbox %s (exit %d): %s", p.Status, p.ExitCode, tail(p.Logs, 800))))
				d.stopRuntime(r)
				return
			}
		}
	}
}

func (d *Driver) current(r *record, rt *runtime) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return r.run == rt && !r.stopped && !r.deleting
}

func (d *Driver) supervisorErr(r *record) string {
	matches, _ := filepath.Glob(filepath.Join(d.sandboxDir(r), "gen-*", "supervisor.err.log"))
	sort.Strings(matches)
	if len(matches) == 0 {
		return "no log"
	}
	b, _ := os.ReadFile(matches[len(matches)-1])
	return tail(string(b), 800)
}

// stopRuntime ends the control side of the current generation.
func (d *Driver) stopRuntime(r *record) {
	d.mu.Lock()
	rt := r.run
	r.run = nil
	d.mu.Unlock()
	if rt == nil {
		return
	}
	rt.liveness.Close() // the supervisor exits on EOF
	if rt.cancel != nil {
		rt.cancel()
	}
	select {
	case <-rt.exited:
	case <-time.After(5 * time.Second):
		rt.supervisor.Process.Kill()
	}
}

// stopWorkload ends the workload side (openshell-sandbox and its tunnel).
func (d *Driver) stopWorkload(ctx context.Context, r *record) {
	if r.blxName == "" {
		return
	}
	for _, p := range []string{sandboxProc, tunnelProc} {
		if err := d.bl.KillProcess(ctx, r.blxName, p); err != nil && !errors.Is(err, blaxel.ErrNotFound) {
			d.log.Debug("kill process", "sandbox", r.name, "process", p, "err", err)
		}
	}
}

func (d *Driver) teardown(ctx context.Context, r *record) {
	d.stopRuntime(r)
	d.stopWorkload(ctx, r)
}

// runLong runs a command that may outlast one sandbox-API request.
func (d *Driver) runLong(ctx context.Context, sandbox, name, script string, timeout time.Duration) error {
	_ = d.bl.KillProcess(ctx, sandbox, name)
	if _, err := d.bl.Exec(ctx, sandbox, blaxel.ProcessRequest{Name: name, Command: "sh -c " + shellQuote(script)}); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		p, err := d.bl.GetProcess(ctx, sandbox, name)
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
