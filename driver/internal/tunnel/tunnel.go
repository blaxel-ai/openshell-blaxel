// Package tunnel carries TCP connections from inside a Blaxel sandbox back to
// a service on the driver host, over one WebSocket that the host dials in.
//
// Blaxel only exposes sandbox ports as authenticated HTTPS/WebSocket, and the
// sandbox must not hold credentials that could reach the gateway host. So the
// host opens the WebSocket (with its Blaxel token), and the sandbox side
// multiplexes each local connection as a yamux stream back to the host:
//
//	sandbox: openshell-sandbox -> 127.0.0.1:17680 -> Serve (yamux client, opens streams)
//	host:    Dial (yamux server, accepts streams) -> gateway 127.0.0.1:<port>
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 15 * time.Second // below Blaxel's idle cutoff (< 300 s observed)
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}

func pipe(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() { a.Close(); b.Close() }
	go func() { io.Copy(a, b); once.Do(closeBoth) }()
	io.Copy(b, a)
	once.Do(closeBoth)
}

// Serve runs inside the sandbox. It accepts the host's WebSocket on wsAddr and
// forwards every connection accepted on localAddr through it. Only one host
// session is active at a time; a new one replaces the old.
func Serve(ctx context.Context, wsAddr, localAddr string, log *slog.Logger) error {
	var mu sync.Mutex
	var session *yamux.Session

	local, err := net.Listen("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", localAddr, err)
	}
	go func() {
		for {
			c, err := local.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			s := session
			mu.Unlock()
			if s == nil || s.IsClosed() {
				log.Warn("no host session; dropping local connection")
				c.Close()
				continue
			}
			go func() {
				stream, err := s.Open()
				if err != nil {
					log.Warn("open stream", "err", err)
					c.Close()
					return
				}
				pipe(c, stream)
			}()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		up := session != nil && !session.IsClosed()
		mu.Unlock()
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintf(w, "session=%v\n", up)
	})
	mux.HandleFunc("/tunnel", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ws.SetReadLimit(-1)
		conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
		s, err := yamux.Client(conn, yamuxConfig())
		if err != nil {
			conn.Close()
			return
		}
		mu.Lock()
		if session != nil {
			session.Close()
		}
		session = s
		mu.Unlock()
		log.Info("host session attached", "remote", r.RemoteAddr)
		<-s.CloseChan()
		log.Info("host session closed")
	})
	srv := &http.Server{Addr: wsAddr, Handler: mux}
	go func() { <-ctx.Done(); srv.Close(); local.Close() }()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Dial runs on the driver host. It keeps a session to url open (reconnecting
// with backoff until ctx ends) and connects each stream to target.
// header returns fresh request headers per attempt so tokens can rotate.
func Dial(ctx context.Context, url string, header func() (http.Header, error), target string, log *slog.Logger, onState func(up bool)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := dialOnce(ctx, url, header, target, log, func() { backoff = time.Second; onState(true) })
		onState(false)
		if ctx.Err() != nil {
			return
		}
		log.Warn("tunnel down, reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func dialOnce(ctx context.Context, url string, header func() (http.Header, error), target string, log *slog.Logger, onUp func()) error {
	h, err := header()
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, resp, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPHeader: h})
	cancel()
	if err != nil {
		if resp != nil {
			return fmt.Errorf("%w (HTTP %s)", err, resp.Status)
		}
		return err
	}
	ws.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	session, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		conn.Close()
		return err
	}
	defer session.Close()
	go func() { <-ctx.Done(); session.Close() }()
	onUp()
	log.Info("tunnel up", "url", url)
	for {
		stream, err := session.Accept()
		if err != nil {
			return err
		}
		go func() {
			up, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				log.Warn("dial target", "target", target, "err", err)
				stream.Close()
				return
			}
			if tc, ok := up.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			pipe(stream, up)
		}()
	}
}
