// Package doh is a minimal DNS forwarder that relays classic DNS (UDP and
// TCP) to an RFC 8484 DNS-over-HTTPS resolver.
//
// Blaxel sandboxes are IPv6-only and only let HTTPS leave the platform, so a
// supervisor that asks its /etc/resolv.conf resolver for A records gets no
// data from the platform's DNS64. This forwarder, listed first in the control
// sandbox's resolv.conf, answers A and AAAA queries from a public DoH
// resolver over port 443. Messages are relayed unchanged.
package doh

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const maxMessage = 65535

type Forwarder struct {
	URL    string // e.g. https://[2606:4700:4700::1111]/dns-query
	Client *http.Client
	Log    *slog.Logger
}

func New(url string, log *slog.Logger) *Forwarder {
	return &Forwarder{URL: url, Client: &http.Client{Timeout: 5 * time.Second}, Log: log}
}

// Exchange sends one DNS message and returns the response message.
func (f *Forwarder) Exchange(ctx context.Context, msg []byte) ([]byte, error) {
	if len(msg) < 12 {
		return nil, errors.New("short DNS message")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH %s: HTTP %d", f.URL, resp.StatusCode)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxMessage || len(out) < 12 {
		return nil, errors.New("invalid DoH response size")
	}
	// DoH resolvers answer with ID 0 when asked with ID 0; keep the caller's ID.
	copy(out[:2], msg[:2])
	return out, nil
}

// servfail builds a SERVFAIL reply for a query so clients fail fast.
func servfail(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	r := append([]byte(nil), q...)
	r[2] |= 0x80                // QR
	r[3] = (r[3] & 0xf0) | 0x02 // RCODE=SERVFAIL
	return r
}

// ServeUDP answers DNS over UDP on addr until ctx ends.
func (f *Forwarder) ServeUDP(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, maxMessage)
	for {
		n, peer, err := pc.ReadFrom(buf)
		if err != nil {
			return ctx.Err()
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			resp, err := f.Exchange(ctx, q)
			if err != nil {
				f.Log.Warn("doh exchange", "err", err)
				resp = servfail(q)
			}
			if resp != nil {
				pc.WriteTo(resp, peer)
			}
		}()
	}
}

// ServeTCP answers DNS over TCP (length-prefixed) on addr until ctx ends.
func (f *Forwarder) ServeTCP(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			return ctx.Err()
		}
		go func() {
			defer c.Close()
			for {
				c.SetDeadline(time.Now().Add(10 * time.Second))
				var n uint16
				if err := binary.Read(c, binary.BigEndian, &n); err != nil {
					return
				}
				q := make([]byte, n)
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				resp, err := f.Exchange(ctx, q)
				if err != nil {
					f.Log.Warn("doh exchange", "err", err)
					resp = servfail(q)
				}
				if resp == nil {
					return
				}
				binary.Write(c, binary.BigEndian, uint16(len(resp)))
				if _, err := c.Write(resp); err != nil {
					return
				}
			}
		}()
	}
}
