// openshell-driver-blaxel is an external OpenShell (v0.0.116) compute driver
// that runs sandboxes as Blaxel microVMs. The gateway connects to it over a
// Unix socket:
//
//	openshell-gateway --drivers blaxel --compute-driver-socket <socket> ...
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
	env := flag.String("env", envOr("BL_ENV", "prod"), "Blaxel environment: prod or dev")
	flag.StringVar(&cfg.Owner, "owner", driver.DefaultOwner, "driver instance identity (use the gateway name); recovery only adopts sandboxes with this owner")
	flag.StringVar(&cfg.Region, "region", "us-was-1", "Blaxel region")
	flag.StringVar(&cfg.DefaultImage, "image", "blaxel/py-app:latest", "Blaxel image used when the request's image is not a Blaxel image")
	flag.IntVar(&cfg.MemoryMiB, "memory", 4096, "default sandbox memory in MiB")
	flag.StringVar(&cfg.GatewayAddr, "gateway-addr", "127.0.0.1:17680", "gateway listener the tunnel forwards to")
	flag.IntVar(&cfg.InVMGatewayPort, "vm-gateway-port", 17680, "loopback port inside the VM that reaches the gateway")
	flag.IntVar(&cfg.TunnelPort, "tunnel-port", 9000, "Blaxel sandbox port for the tunnel WebSocket")
	flag.StringVar(&cfg.TLSDir, "tls-dir", "", "gateway TLS dir with ca.crt and client/tls.{crt,key}")
	flag.StringVar(&cfg.SandboxBinary, "sandbox-bin", "", "linux x86_64 openshell-sandbox v0.0.116")
	flag.StringVar(&cfg.TunnelBinary, "tunnel-bin", "", "linux x86_64 os-tunnel")
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
	for name, v := range map[string]string{"-workspace": *workspace, "-tls-dir": cfg.TLSDir, "-sandbox-bin": cfg.SandboxBinary, "-tunnel-bin": cfg.TunnelBinary} {
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
