// openshell-driver-blaxel is an external OpenShell (main, RFC 0012) compute
// driver that runs sandboxes as Blaxel microVMs. It runs next to the gateway
// in a Blaxel control sandbox, serves the gateway over a Unix socket, and
// spawns one openshell-supervisor per sandbox:
//
//	[openshell.gateway] compute_driver = "blaxel"
//	[openshell.drivers.blaxel] socket_path = "<socket>"
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"google.golang.org/grpc"

	pb "github.com/blaxel-ai/openshell-blaxel/driver/gen/computev1"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/blaxel"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/driver"
)

func main() {
	var cfg driver.Config
	socket := flag.String("socket", "/tmp/openshell-blaxel/driver.sock", "Unix socket the gateway connects to")
	workspace := flag.String("workspace", os.Getenv("BL_WORKSPACE"), "Blaxel workspace")
	env := flag.String("env", envOr("BL_ENV", "prod"), "Blaxel environment")
	flag.StringVar(&cfg.Owner, "owner", driver.DefaultOwner, "driver instance identity (use the gateway name); recovery only adopts sandboxes with this owner")
	flag.StringVar(&cfg.Region, "region", "us-was-1", "Blaxel region")
	flag.StringVar(&cfg.DefaultImage, "image", "blaxel/py-app:latest", "Blaxel image used when the request's image is not a Blaxel image")
	flag.IntVar(&cfg.MemoryMiB, "memory", 4096, "default sandbox memory in MiB")
	flag.IntVar(&cfg.TunnelPort, "tunnel-port", 9000, "workload sandbox port for the supervisor tunnel")
	flag.StringVar(&cfg.SandboxBinary, "sandbox-bin", "", "linux x86_64 openshell-sandbox (OpenShell main), uploaded into workloads")
	flag.StringVar(&cfg.TunnelBinary, "tunnel-bin", "", "linux x86_64 os-tunnel, uploaded into workloads")
	flag.StringVar(&cfg.SupervisorBinary, "supervisor-bin", "", "openshell-supervisor (OpenShell main), run next to the gateway")
	flag.StringVar(&cfg.StateDir, "state-dir", "/var/lib/openshell-blaxel", "per-sandbox descriptors, auth bundles and supervisor logs")
	flag.StringVar(&cfg.GatewayEndpoint, "gateway-endpoint", "https://127.0.0.1:17670", "gateway URL for supervisors")
	tlsDir := flag.String("gateway-tls-dir", "", "gateway PKI dir (ca.crt, client/tls.{crt,key}) for supervisor mTLS")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "default OpenShell log level for sandboxes")
	packages := flag.String("packages", "curl,git,ca-certificates", "comma-separated packages installed as root in every sandbox")
	flag.BoolVar(&cfg.InstallClaude, "install-claude", true, "install Claude Code at /usr/local/bin/claude in every sandbox")
	kernel := flag.String("kernel-variant", "landlock", "Blaxel kernel variant enabled via spec.runtime.extraArgs (empty for the default kernel)")
	flag.Parse()
	if *kernel != "" {
		cfg.ExtraArgs = map[string]string{*kernel: "enabled"}
	}
	for _, p := range strings.Split(*packages, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if !validPackage.MatchString(p) {
				fmt.Fprintf(os.Stderr, "invalid package name %q\n", p)
				os.Exit(2)
			}
			cfg.Packages = append(cfg.Packages, p)
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *tlsDir != "" {
		cfg.GatewayCA = filepath.Join(*tlsDir, "ca.crt")
		cfg.GatewayCert = filepath.Join(*tlsDir, "client", "tls.crt")
		cfg.GatewayKey = filepath.Join(*tlsDir, "client", "tls.key")
	}
	for name, v := range map[string]string{"-workspace": *workspace, "-sandbox-bin": cfg.SandboxBinary, "-tunnel-bin": cfg.TunnelBinary,
		"-supervisor-bin": cfg.SupervisorBinary, "-gateway-tls-dir": *tlsDir} {
		if v == "" {
			fmt.Fprintf(os.Stderr, "%s is required\n", name)
			os.Exit(2)
		}
	}

	bl, err := blaxel.NewClient(*workspace, *env)
	if err != nil {
		log.Error("blaxel", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		log.Error("state dir", "err", err)
		os.Exit(1)
	}
	d := driver.New(cfg, bl, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := d.Recover(ctx); err != nil {
		log.Warn("recovery from Blaxel failed; starting empty", "err", err)
	}

	lis, err := listenPrivate(*socket)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	// Remove only our own socket: a replacement driver may already have bound
	// the same path while this one drains.
	ours, _ := os.Lstat(*socket)
	defer func() {
		if cur, err := os.Lstat(*socket); err == nil && ours != nil && os.SameFile(ours, cur) {
			os.Remove(*socket)
		}
	}()
	srv := grpc.NewServer()
	pb.RegisterComputeDriverServer(srv, d)
	go func() { <-ctx.Done(); d.Close(); srv.GracefulStop() }()
	log.Info("openshell-driver-blaxel serving", "socket", *socket, "workspace", *workspace, "env", *env, "region", cfg.Region)
	if err := srv.Serve(lis); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// listenPrivate binds a Unix socket only the current user can reach (the
// socket inherits no authentication, so file permissions are the boundary).
func listenPrivate(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path)
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return lis, os.Chmod(path, 0o600)
}

// validPackage keeps -packages values safe to splice into the setup script.
var validPackage = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
