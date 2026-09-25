// os-deploy stands up the OpenShell control plane in a Blaxel sandbox: the
// OpenShell (main) gateway, the Blaxel compute driver, and the supervisors it
// spawns. Workload sandboxes are created by the driver on demand.
//
//	os-deploy up     -bin ../bin/main -driver bin -config ~/.openshell-blaxel
//	os-deploy status
//	os-deploy down
//
// The control sandbox gets the service-account key (BL_API_KEY) in a
// root-only file read by the driver process only. The laptop gets the
// gateway CA and client certificate, and reaches the gateway through
// `os-tunnel dial`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaxel-ai/openshell-blaxel/driver/internal/blaxel"
)

const (
	optBin      = "/opt/openshell/bin"
	optWorkload = "/opt/openshell/workload"
	varDir      = "/var/lib/openshell"
	tlsDir      = varDir + "/tls"
	gatewayTOML = varDir + "/gateway.toml"
	driverEnv   = varDir + "/driver.env"
	driverSock  = "/run/openshell/blaxel.sock"
	clatSetup   = varDir + "/clat.sh"
	gatewayPort = 17670
	ingressPort = 9000
)

type opts struct {
	name, workspace, env, region, owner string
	mainBin, driverBin, configDir       string
	memory                              int
	kernel                              string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: os-deploy up|status|down [flags]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.name, "name", "os-control", "control sandbox name")
	fs.StringVar(&o.workspace, "workspace", os.Getenv("BL_WORKSPACE"), "Blaxel workspace")
	fs.StringVar(&o.env, "env", envOr("BL_ENV", "prod"), "Blaxel environment (prod or dev)")
	fs.StringVar(&o.region, "region", envOr("BL_REGION", "us-was-1"), "Blaxel region")
	fs.StringVar(&o.owner, "owner", "blaxel-main", "driver owner label (one per control plane)")
	fs.StringVar(&o.mainBin, "bin", "bin/main", "dir with OpenShell main linux binaries: openshell-gateway, openshell-supervisor, openshell-sandbox")
	fs.StringVar(&o.driverBin, "driver", "driver/bin", "dir with openshell-driver-blaxel-linux-amd64 and os-tunnel-linux-amd64")
	fs.StringVar(&o.configDir, "config", filepath.Join(home(), ".openshell-blaxel"), "local dir for the gateway client bundle")
	fs.IntVar(&o.memory, "memory", 4096, "control sandbox memory (MiB)")
	fs.StringVar(&o.kernel, "workload-kernel", "landlock", "kernel variant for workload sandboxes")
	fs.Parse(os.Args[2:])

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if o.workspace == "" {
		fail(log, errors.New("set -workspace or BL_WORKSPACE"))
	}
	bl, err := blaxel.NewClient(o.workspace, o.env)
	if err != nil {
		fail(log, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	switch cmd {
	case "up":
		err = up(ctx, bl, o, log)
	case "status":
		err = status(ctx, bl, o)
	case "down":
		err = bl.DeleteSandbox(ctx, o.name)
		if err == nil {
			fmt.Println("deleted control sandbox", o.name, "(workload sandboxes are left running; delete them first through OpenShell)")
		}
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fail(log, err)
	}
}

func up(ctx context.Context, bl *blaxel.Client, o opts, log *slog.Logger) error {
	apiKey := os.Getenv("BL_API_KEY")
	if apiKey == "" {
		return errors.New("BL_API_KEY (a service-account key) is required: the control sandbox uses it to create workload sandboxes")
	}
	sb, err := bl.GetSandbox(ctx, o.name)
	// A deleted sandbox stays listed as TERMINATED; creating over the name
	// revives it as a new sandbox, so treat it like a missing one.
	if err == nil && isGone(sb.Status) {
		err = blaxel.ErrNotFound
	}
	if errors.Is(err, blaxel.ErrNotFound) {
		log.Info("creating control sandbox", "name", o.name, "region", o.region)
		sb, err = bl.CreateSandbox(ctx, blaxel.SandboxSpec{
			Name: o.name, Region: o.region, Image: "blaxel/py-app:latest", MemoryMiB: o.memory,
			Labels: map[string]string{"openshell.ai/role": "control-plane", "openshell.ai/driver-owner": o.owner},
			Ports:  []blaxel.Port{{Target: ingressPort, Protocol: "HTTP"}},
			// tun + iptables kernels for the IPv4 CLAT (see clatScript).
			ExtraArgs: map[string]string{"tun": "enabled", "iptables": "enabled"},
		})
	}
	if err != nil {
		return err
	}
	if err := waitAPI(ctx, bl, o.name); err != nil {
		return err
	}

	// Stop the control plane first: running binaries can't be overwritten
	// ("text file busy"). Supervisors exit with the driver.
	for _, p := range []string{"os-ingress", "os-gateway", "os-driver", "os-dns"} {
		_ = bl.KillProcess(ctx, o.name, p)
	}
	time.Sleep(2 * time.Second)

	log.Info("uploading binaries")
	uploads := map[string]string{
		filepath.Join(o.mainBin, "openshell-gateway"):                     optBin + "/openshell-gateway",
		filepath.Join(o.mainBin, "openshell-supervisor"):                  optBin + "/openshell-supervisor",
		filepath.Join(o.driverBin, "openshell-driver-blaxel-linux-amd64"): optBin + "/openshell-driver-blaxel",
		filepath.Join(o.driverBin, "os-tunnel-linux-amd64"):               optBin + "/os-tunnel",
		filepath.Join(o.mainBin, "openshell-sandbox"):                     optWorkload + "/openshell-sandbox",
	}
	for src, dst := range uploads {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := bl.WriteFile(ctx, o.name, dst, data, "0755"); err != nil {
			return fmt.Errorf("upload %s: %w", dst, err)
		}
	}
	env := fmt.Sprintf("BL_API_KEY=%s\nBL_WORKSPACE=%s\nBL_ENV=%s\n", apiKey, o.workspace, o.env)
	if err := bl.WriteFile(ctx, o.name, driverEnv, []byte(env), "0600"); err != nil {
		return err
	}
	if err := bl.WriteFile(ctx, o.name, gatewayTOML, []byte(gatewayConfig()), "0644"); err != nil {
		return err
	}

	log.Info("preparing control sandbox (PKI, directories)")
	prep := `set -e
chmod 755 ` + optBin + `/* ` + optWorkload + `/*
chmod 600 ` + driverEnv + `
mkdir -p /run/openshell /var/lib/openshell-blaxel ` + varDir + `
if [ ! -f ` + tlsDir + `/ca.crt ]; then ` + optBin + `/openshell-gateway generate-certs --output-dir ` + tlsDir + ` >/dev/null; fi
echo prepared`
	if out, err := bl.Run(ctx, o.name, "sh -c "+quote(prep)); err != nil {
		return fmt.Errorf("prepare: %w: %s", err, out)
	}
	log.Info("configuring IPv4 egress (464XLAT CLAT over Blaxel NAT64)")
	if err := bl.WriteFile(ctx, o.name, clatSetup, []byte(clatScript()), "0700"); err != nil {
		return err
	}
	if out, err := bl.Run(ctx, o.name, "sh "+clatSetup+" install"); err != nil {
		return fmt.Errorf("CLAT install: %w: %s", err, out)
	}
	_ = bl.KillProcess(ctx, o.name, "os-dns")
	if _, err := bl.Exec(ctx, o.name, blaxel.ProcessRequest{Name: "os-dns", Command: optBin + "/os-tunnel dns -listen 127.0.0.2:53", KeepAlive: true, Timeout: blaxel.Forever}); err != nil {
		return fmt.Errorf("start DNS forwarder: %w", err)
	}
	_ = bl.KillProcess(ctx, o.name, "os-clat")
	if _, err := bl.Exec(ctx, o.name, blaxel.ProcessRequest{Name: "os-clat", Command: "sh " + clatSetup + " run", KeepAlive: true, Timeout: blaxel.Forever}); err != nil {
		return fmt.Errorf("start CLAT: %w", err)
	}
	if err := waitIPv4(ctx, bl, o.name); err != nil {
		return err
	}

	cp := fmt.Sprintf("cp %s/os-tunnel %s/os-tunnel", optBin, optWorkload)
	if _, err := bl.Run(ctx, o.name, cp); err != nil {
		return err
	}

	log.Info("starting driver, gateway and ingress tunnel")
	driverCmd := `set -a; . ` + driverEnv + `; set +a; exec ` + optBin + `/openshell-driver-blaxel` +
		` -socket ` + driverSock + ` -owner ` + o.owner + ` -workspace "$BL_WORKSPACE" -env "$BL_ENV" -region ` + o.region +
		` -kernel-variant ` + o.kernel +
		` -sandbox-bin ` + optWorkload + `/openshell-sandbox -tunnel-bin ` + optWorkload + `/os-tunnel` +
		` -supervisor-bin ` + optBin + `/openshell-supervisor -gateway-tls-dir ` + tlsDir +
		fmt.Sprintf(` -gateway-endpoint https://127.0.0.1:%d`, gatewayPort) +
		` -state-dir /var/lib/openshell-blaxel`
	gatewayCmd := `for i in $(seq 100); do [ -S ` + driverSock + ` ] && break; sleep 0.2; done; exec env -u BL_API_KEY ` + optBin +
		`/openshell-gateway --config ` + gatewayTOML + ` --db-url 'sqlite:` + varDir + `/gateway.db?mode=rwc' --log-level info`
	ingressCmd := fmt.Sprintf(`exec %s/os-tunnel serve -ws :%d -target tcp:127.0.0.1:%d`, optBin, ingressPort, gatewayPort)
	for _, p := range []struct{ name, cmd string }{{"os-driver", driverCmd}, {"os-gateway", gatewayCmd}, {"os-ingress", ingressCmd}} {
		if _, err := bl.Exec(ctx, o.name, blaxel.ProcessRequest{Name: p.name, Command: "sh -c " + quote(p.cmd), KeepAlive: true, Timeout: blaxel.Forever}); err != nil {
			return fmt.Errorf("start %s: %w", p.name, err)
		}
	}
	if err := waitGateway(ctx, bl, o.name); err != nil {
		return err
	}

	log.Info("fetching the gateway client bundle", "dir", o.configDir)
	mtls := filepath.Join(o.configDir, "mtls")
	if err := os.MkdirAll(mtls, 0o700); err != nil {
		return err
	}
	for src, dst := range map[string]string{tlsDir + "/ca.crt": "ca.crt", tlsDir + "/client/tls.crt": "tls.crt", tlsDir + "/client/tls.key": "tls.key"} {
		b, err := bl.ReadFile(ctx, o.name, src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(mtls, dst), b, 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("\ncontrol plane ready in sandbox %s (%s)\nclient bundle: %s\n", o.name, sb.URL, mtls)
	return nil
}

// clatScript gives the control sandbox IPv4 egress on Blaxel's IPv6-only
// network: tayga translates IPv4 to IPv6 through the platform NAT64 prefix
// (discovered with RFC 7050), and translated packets leave through the VM's
// single global IPv6 address via MASQUERADE. OpenShell main's supervisor
// resolves and dials IPv4 only (its mediated policy DNS disables IPv6
// egress), so this is what makes policy-allowed egress work. Workload
// sandboxes are unaffected: their only interface is loopback.
//
//	clat.sh install   packages, tayga.conf, NAT, resolv.conf (idempotent)
//	clat.sh run       tayga in the foreground (supervised by Blaxel)
func clatScript() string {
	return `#!/bin/sh
set -e
ULA=fd64:c1a7:0:1
case "$1" in
install)
  if ! command -v tayga >/dev/null || ! command -v ip6tables >/dev/null; then
    apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends tayga iproute2 iptables >/dev/null
  fi
  PREFIX=$(python3 -c "
import socket, ipaddress
a = sorted({x[4][0] for x in socket.getaddrinfo('ipv4only.arpa', 0, socket.AF_INET6)})[0]
n = ipaddress.IPv6Network(a + '/96', strict=False)
print(n)")
  [ -n "$PREFIX" ] || { echo "no NAT64 prefix (ipv4only.arpa has no AAAA)"; exit 1; }
  cat > /etc/tayga.conf <<CONF
tun-device clat
ipv4-addr 192.0.0.1
ipv6-addr $ULA::1
prefix $PREFIX
map 192.0.0.2 $ULA::2
data-dir /var/lib/tayga
CONF
  mkdir -p /var/lib/tayga
  ip link show clat >/dev/null 2>&1 || tayga --mktun
  ip link set clat up mtu 1480
  ip addr replace 192.0.0.2/32 dev clat
  ip route replace 192.0.0.1/32 dev clat
  ip route replace default dev clat mtu 1480
  ip -6 route replace $ULA::/96 dev clat
  # Forwarding disables RA-learned routes unless accept_ra=2.
  echo 2 > /proc/sys/net/ipv6/conf/eth0/accept_ra
  echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
  echo 1 > /proc/sys/net/ipv4/ip_forward
  ip6tables -t nat -C POSTROUTING -s $ULA::/96 -o eth0 -j MASQUERADE 2>/dev/null ||
    ip6tables -t nat -A POSTROUTING -s $ULA::/96 -o eth0 -j MASQUERADE
  # The supervisor asks its first nameserver for A records. Platform DNS64
  # has none and only HTTPS leaves Blaxel, so the first nameserver is the
  # local DNS-over-HTTPS forwarder (os-tunnel dns, process os-dns).
  grep -v '^nameserver 1.1.1.1$' /etc/resolv.conf | grep -v '^nameserver 127.0.0.2$' > /etc/resolv.conf.clat
  { printf 'nameserver 127.0.0.2\n'; cat /etc/resolv.conf.clat; } > /etc/resolv.conf
  echo "clat installed: prefix $PREFIX"
  ;;
run)
  exec tayga -d -c /etc/tayga.conf
  ;;
esac
`
}

func waitIPv4(ctx context.Context, bl *blaxel.Client, name string) error {
	check := `python3 -c "import socket;socket.create_connection(('1.1.1.1',443),4).close();print(socket.gethostbyname('api.github.com'))"`
	var last string
	for i := 0; i < 20; i++ {
		out, err := bl.Run(ctx, name, check)
		if err == nil {
			return nil
		}
		last = out
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("IPv4 egress through the CLAT did not come up: %s", tailStr(last, 500))
}

func gatewayConfig() string {
	return fmt.Sprintf(`# Generated by os-deploy.
[openshell]
version = 2

[openshell.gateway]
bind_address = "127.0.0.1:%d"
compute_driver = "blaxel"

[openshell.gateway.tls]
cert_path = "%[2]s/server/tls.crt"
key_path = "%[2]s/server/tls.key"
client_ca_path = "%[2]s/ca.crt"

[openshell.gateway.mtls_auth]
enabled = true

[openshell.gateway.gateway_jwt]
signing_key_path = "%[2]s/jwt/signing.pem"
public_key_path = "%[2]s/jwt/public.pem"
kid_path = "%[2]s/jwt/kid"
gateway_id = "openshell"

[openshell.drivers.blaxel]
socket_path = "%[3]s"
`, gatewayPort, tlsDir, driverSock)
}

func waitAPI(ctx context.Context, bl *blaxel.Client, name string) error {
	for i := 0; i < 60; i++ {
		if _, err := bl.Run(ctx, name, "true"); err == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("control sandbox %s did not become reachable", name)
}

func waitGateway(ctx context.Context, bl *blaxel.Client, name string) error {
	check := fmt.Sprintf(`python3 -c "import socket;socket.create_connection(('127.0.0.1',%d),2)"`, gatewayPort)
	for i := 0; i < 40; i++ {
		if _, err := bl.Run(ctx, name, check); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	logs, _ := bl.GetProcess(ctx, name, "os-gateway")
	dlogs, _ := bl.GetProcess(ctx, name, "os-driver")
	msg := ""
	if logs != nil {
		msg += "\n--- gateway:\n" + tailStr(logs.Logs, 1500)
	}
	if dlogs != nil {
		msg += "\n--- driver:\n" + tailStr(dlogs.Logs, 1500)
	}
	return errors.New("gateway did not start listening" + msg)
}

func status(ctx context.Context, bl *blaxel.Client, o opts) error {
	sb, err := bl.GetSandbox(ctx, o.name)
	if err != nil {
		return err
	}
	fmt.Printf("control sandbox %s: %s %s\n", sb.Name, sb.Status, sb.URL)
	for _, p := range []string{"os-clat", "os-dns", "os-driver", "os-gateway", "os-ingress"} {
		pr, err := bl.GetProcess(ctx, o.name, p)
		if err != nil {
			fmt.Printf("  %-11s %v\n", p, err)
			continue
		}
		fmt.Printf("  %-11s %s\n", p, pr.Status)
	}
	return nil
}

func isGone(status string) bool {
	switch strings.ToUpper(status) {
	case "TERMINATED", "TERMINATING", "DELETING", "FAILED":
		return true
	}
	return false
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func tailStr(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func home() string { h, _ := os.UserHomeDir(); return h }

func fail(log *slog.Logger, err error) {
	log.Error("os-deploy", "err", err)
	os.Exit(1)
}
