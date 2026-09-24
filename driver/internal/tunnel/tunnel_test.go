package tunnel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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

// TestReverseTunnelRoundTrip runs both halves locally: connections accepted
// on the "in-VM" loopback address must reach the "host" target through one
// WebSocket session, concurrently and byte-exact.
func TestReverseTunnelRoundTrip(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Host-side target: an echo server standing in for the gateway.
	target, err := net.Listen("tcp", "127.0.0.1:0")
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

	wsAddr, localAddr := freePort(t), freePort(t)
	go Serve(ctx, wsAddr, localAddr, log)

	up := make(chan struct{}, 1)
	go Dial(ctx, "ws://"+wsAddr+"/tunnel", func() (http.Header, error) { return http.Header{}, nil },
		target.Addr().String(), log, func(ok bool) {
			if ok {
				select {
				case up <- struct{}{}:
				default:
				}
			}
		})
	select {
	case <-up:
	case <-time.After(10 * time.Second):
		t.Fatal("tunnel did not come up")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
			if err != nil {
				t.Errorf("dial local: %v", err)
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

// TestServeDropsConnectionsWithoutHostSession checks that nothing inside the
// VM can reach the host before the driver has attached.
func TestServeDropsConnectionsWithoutHostSession(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsAddr, localAddr := freePort(t), freePort(t)
	go Serve(ctx, wsAddr, localAddr, log)

	var c net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if c, err = net.Dial("tcp", localAddr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("expected the connection to be closed, got %v", err)
	}
}
