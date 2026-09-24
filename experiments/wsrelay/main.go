// wsrelay bridges a byte stream over a WebSocket so a raw TCP/Unix channel can
// cross Blaxel's HTTPS-only sandbox ingress.
//
//	wsrelay serve -listen :9000 -target unix:/run/os/boundary.sock   (in the sandbox)
//	wsrelay dial  -url wss://.../port/9000 -listen 127.0.0.1:19000   (on the host)
//	wsrelay echo  -listen 127.0.0.1:9001                              (test target)
//
// Each accepted local connection on the dial side opens one WebSocket; each
// WebSocket on the serve side opens one connection to the target.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func splitTarget(t string) (network, addr string) {
	if rest, ok := strings.CutPrefix(t, "unix:"); ok {
		return "unix", rest
	}
	return "tcp", strings.TrimPrefix(t, "tcp:")
}

// keepalive pings the peer so idle tunnels survive proxy idle timeouts.
func keepalive(ws *websocket.Conn, every time.Duration) {
	for {
		time.Sleep(every)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := ws.Ping(ctx)
		cancel()
		if err != nil {
			return
		}
	}
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
}

func serve(listen, target string) {
	network, addr := splitTarget(target)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ctx := context.Background()
		ws.SetReadLimit(-1)
		up, err := net.DialTimeout(network, addr, 5*time.Second)
		if err != nil {
			ws.Close(websocket.StatusInternalError, "target unreachable")
			return
		}
		log.Printf("serve: %s -> %s", r.RemoteAddr, target)
		pipe(websocket.NetConn(ctx, ws, websocket.MessageBinary), up)
	})
	log.Printf("serve: listening on %s, target %s", listen, target)
	log.Fatal(http.ListenAndServe(listen, nil))
}

func dial(url, listen string) {
	header := http.Header{}
	if tok := os.Getenv("WSRELAY_BEARER"); tok != "" {
		header.Set("Authorization", "Bearer "+tok)
	}
	if tok := os.Getenv("WSRELAY_PREVIEW_TOKEN"); tok != "" {
		header.Set("X-Blaxel-Preview-Token", tok)
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("dial: %s -> %s", listen, url)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
			cancel()
			if err != nil {
				status := ""
				if resp != nil {
					status = resp.Status
				}
				log.Printf("dial: websocket failed: %v %s", err, status)
				c.Close()
				return
			}
			ws.SetReadLimit(-1)
			go keepalive(ws, *pingEvery)
			pipe(c, websocket.NetConn(context.Background(), ws, websocket.MessageBinary))
		}()
	}
}

func echo(listen string) {
	network, addr := splitTarget(listen)
	if network == "unix" {
		os.Remove(addr)
	}
	l, err := net.Listen(network, addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("echo: listening on %s", listen)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() { io.Copy(c, c); c.Close() }()
	}
}

var pingEvery *time.Duration

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: wsrelay serve|dial|echo [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	listen := fs.String("listen", "", "listen address")
	target := fs.String("target", "", "serve: tcp:host:port or unix:/path")
	url := fs.String("url", "", "dial: websocket URL")
	pingEvery = fs.Duration("ping", 20*time.Second, "dial: websocket ping interval")
	fs.Parse(os.Args[2:])
	switch os.Args[1] {
	case "serve":
		serve(*listen, *target)
	case "dial":
		dial(*url, *listen)
	case "echo":
		echo(*listen)
	}
}
