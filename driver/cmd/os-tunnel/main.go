// os-tunnel carries TCP/Unix byte streams across Blaxel's WebSocket ingress.
//
//	os-tunnel serve -ws :9000 -target unix:/run/openshell/boundary.sock   (in a sandbox)
//	os-tunnel dial  -sandbox os-control -listen 127.0.0.1:17670           (laptop -> control gateway)
//	os-tunnel dns   -listen 127.0.0.2:53                                   (DNS -> DNS-over-HTTPS)
//
// serve accepts one multiplexed session on /tunnel and connects each stream
// to -target. dial authenticates with the Blaxel SDK (BL_API_KEY or the
// `bl login` session), dials <sandbox URL>/port/<port>/tunnel and exposes the
// streams on a local listener.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/blaxel-ai/openshell-blaxel/driver/internal/blaxel"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/doh"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/tunnel"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		ws := fs.String("ws", ":9000", "address for the control side's WebSocket")
		target := fs.String("target", "unix:/run/openshell/boundary.sock", "unix:/path or tcp:host:port")
		fs.Parse(os.Args[2:])
		if err := tunnel.Serve(ctx, *ws, *target, log); err != nil {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	case "dial":
		fs := flag.NewFlagSet("dial", flag.ExitOnError)
		sandbox := fs.String("sandbox", "", "Blaxel sandbox to connect to")
		port := fs.Int("port", 9000, "sandbox port os-tunnel serve listens on")
		listen := fs.String("listen", "127.0.0.1:17670", "local listen address")
		workspace := fs.String("workspace", os.Getenv("BL_WORKSPACE"), "Blaxel workspace")
		env := fs.String("env", os.Getenv("BL_ENV"), "Blaxel environment (prod or dev)")
		fs.Parse(os.Args[2:])
		if *sandbox == "" || *workspace == "" {
			fmt.Fprintln(os.Stderr, "dial needs -sandbox and -workspace (or BL_WORKSPACE)")
			os.Exit(2)
		}
		bl, err := blaxel.NewClient(*workspace, *env)
		if err != nil {
			log.Error("blaxel", "err", err)
			os.Exit(1)
		}
		sb, err := bl.GetSandbox(ctx, *sandbox)
		if err != nil {
			log.Error("get sandbox", "sandbox", *sandbox, "err", err)
			os.Exit(1)
		}
		ep, err := tunnel.Listen(*listen)
		if err != nil {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
		url := strings.Replace(sb.URL, "https://", "wss://", 1) + fmt.Sprintf("/port/%d/tunnel", *port)
		log.Info("forwarding", "listen", ep.Addr(), "to", url)
		ep.Run(ctx, url, bl.Headers, log)
	case "dns":
		fs := flag.NewFlagSet("dns", flag.ExitOnError)
		listen := fs.String("listen", "127.0.0.2:53", "UDP and TCP address to answer DNS on")
		upstream := fs.String("doh", "https://[2606:4700:4700::1111]/dns-query", "RFC 8484 DNS-over-HTTPS resolver")
		fs.Parse(os.Args[2:])
		f := doh.New(*upstream, log)
		go func() {
			if err := f.ServeTCP(ctx, *listen); err != nil && ctx.Err() == nil {
				log.Error("dns tcp", "err", err)
				os.Exit(1)
			}
		}()
		log.Info("dns forwarder", "listen", *listen, "doh", *upstream)
		if err := f.ServeUDP(ctx, *listen); err != nil && ctx.Err() == nil {
			log.Error("dns udp", "err", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: os-tunnel serve|dial|dns [flags]")
	os.Exit(2)
}
