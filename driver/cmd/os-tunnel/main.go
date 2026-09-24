// os-tunnel is the in-sandbox half of the reverse tunnel. It is uploaded into
// each Blaxel sandbox by the driver and run as:
//
//	os-tunnel -ws :9000 -local 127.0.0.1:17680
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/blaxel-ai/openshell-blaxel/driver/internal/tunnel"
)

func main() {
	wsAddr := flag.String("ws", ":9000", "address for the host's WebSocket")
	local := flag.String("local", "127.0.0.1:17680", "local address forwarded to the host")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := tunnel.Serve(ctx, *wsAddr, *local, log); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
