package tunnel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// TestTunnelToUnixTarget runs both halves locally: connections accepted on the
// control-side loopback port must reach a Unix-socket target (standing in for
// openshell-sandbox's boundary listener) concurrently and byte-exact.
func TestTunnelToUnixTarget(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sock := filepath.Join(t.TempDir(), "boundary.sock")
	target, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	wsAddr := freePort(t)
	go Serve(ctx, wsAddr, "unix:"+sock, log)

	ep, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go ep.Run(ctx, "ws://"+wsAddr+"/tunnel", func() (http.Header, error) { return http.Header{}, nil }, log)
	select {
	case <-ep.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("tunnel did not come up")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", ep.Addr().String(), 5*time.Second)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			defer c.Close()
			msg := []byte(fmt.Sprintf("stream-%d-%s", i, string(make([]byte, 64<<10))))
			go c.Write(msg)
			got := make([]byte, len(msg))
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(c, got); err != nil || string(got) != string(msg) {
				t.Errorf("stream %d: round trip mismatch (%v)", i, err)
			}
		}(i)
	}
	wg.Wait()
}

// TestEndpointDropsConnectionsWhileDisconnected: nothing reaches the workload
// before the tunnel is attached.
func TestEndpointDropsConnectionsWhileDisconnected(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ep, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go ep.Run(ctx, "ws://127.0.0.1:1/tunnel", func() (http.Header, error) { return http.Header{}, nil }, log)
	c, err := net.Dial("tcp", ep.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("expected the connection to be closed, got %v", err)
	}
}
