// Package tunnel carries the OpenShell Sandbox Protocol from the supervisor
// (in the Blaxel control sandbox) to openshell-sandbox (in a workload sandbox),
// over one WebSocket that the control side dials in.
//
// Blaxel exposes sandbox ports only as authenticated HTTPS/WebSocket, and the
// workload VM must hold no credentials, so the control side dials the
// WebSocket with its Blaxel token and multiplexes each supervisor connection
// as a yamux stream:
//
//	control:  supervisor -> 127.0.0.1:<port> -> Dial (yamux client, opens streams)
//	workload: Serve (yamux server, accepts streams) -> unix:/run/openshell/boundary.sock
//
// The Sandbox Protocol is TLS 1.3 end to end inside each stream, so neither
// Blaxel's edge nor this tunnel can read or alter it.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
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

// splitTarget parses "unix:/path" or "tcp:host:port" (or a bare host:port).
func splitTarget(t string) (network, addr string) {
	if rest, ok := strings.CutPrefix(t, "unix:"); ok {
		return "unix", rest
	}
	return "tcp", strings.TrimPrefix(t, "tcp:")
}

// Serve runs in the workload sandbox. It accepts the control side's WebSocket
// on wsAddr and connects every stream to target. A new session replaces the
// previous one, so a restarted driver can re-attach.
func Serve(ctx context.Context, wsAddr, target string, log *slog.Logger) error {
	network, addr := splitTarget(target)
	var mu sync.Mutex
	var current *yamux.Session

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		up := current != nil && !current.IsClosed()
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
		session, err := yamux.Server(conn, yamuxConfig())
		if err != nil {
			conn.Close()
			return
		}
		mu.Lock()
		if current != nil {
			current.Close()
		}
		current = session
		mu.Unlock()
		log.Info("control session attached", "remote", r.RemoteAddr)
		for {
			stream, err := session.Accept()
			if err != nil {
				log.Info("control session closed", "err", err)
				return
			}
			go func() {
				up, err := net.DialTimeout(network, addr, 5*time.Second)
				if err != nil {
					log.Warn("dial target", "target", target, "err", err)
					stream.Close()
					return
				}
				pipe(stream, up)
			}()
		}
	})
	srv := &http.Server{Addr: wsAddr, Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Endpoint is the control side of one tunnel: a local listener whose
// connections are carried to the workload sandbox.
type Endpoint struct {
	listener net.Listener

	mu      sync.Mutex
	session *yamux.Session
	ready   chan struct{}
}

// Addr is the loopback address the supervisor dials.
func (e *Endpoint) Addr() *net.TCPAddr { return e.listener.Addr().(*net.TCPAddr) }

// Listen binds a loopback port for one workload (port 0 picks a free one).
func Listen(addr string) (*Endpoint, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Endpoint{listener: l, ready: make(chan struct{})}, nil
}

// Ready is closed once the first session is up.
func (e *Endpoint) Ready() <-chan struct{} { return e.ready }

// Run keeps a session to url open (reconnecting with backoff until ctx ends)
// and forwards accepted local connections through it. header returns fresh
// request headers per attempt so tokens can rotate.
func (e *Endpoint) Run(ctx context.Context, url string, header func() (http.Header, error), log *slog.Logger) {
	go func() { <-ctx.Done(); e.listener.Close() }()
	go e.accept(log)
	var once sync.Once
	backoff := time.Second
	for ctx.Err() == nil {
		err := e.dialOnce(ctx, url, header, func() {
			backoff = time.Second
			once.Do(func() { close(e.ready) })
			log.Info("tunnel up", "url", url)
		})
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

func (e *Endpoint) accept(log *slog.Logger) {
	for {
		c, err := e.listener.Accept()
		if err != nil {
			return
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		e.mu.Lock()
		s := e.session
		e.mu.Unlock()
		if s == nil || s.IsClosed() {
			log.Warn("tunnel not connected; dropping connection")
			c.Close()
			continue
		}
		go func() {
			stream, err := s.Open()
			if err != nil {
				c.Close()
				return
			}
			pipe(c, stream)
		}()
	}
}

func (e *Endpoint) dialOnce(ctx context.Context, url string, header func() (http.Header, error), onUp func()) error {
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
	session, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		conn.Close()
		return err
	}
	e.mu.Lock()
	e.session = session
	e.mu.Unlock()
	onUp()
	select {
	case <-ctx.Done():
		session.Close()
		return ctx.Err()
	case <-session.CloseChan():
		return errors.New("session closed")
	}
}
