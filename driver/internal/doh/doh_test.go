package doh

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A fake DoH resolver that echoes the query with QR set and ID zeroed, like
// resolvers that answer ID-0 style.
func fakeResolver(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/dns-message" || r.Method != http.MethodPost {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		q, _ := io.ReadAll(r.Body)
		q[0], q[1] = 0, 0
		q[2] |= 0x80
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(q)
	}))
}

func query(id uint16) []byte {
	m := make([]byte, 12)
	binary.BigEndian.PutUint16(m[0:2], id)
	m[2] = 0x01 // RD
	binary.BigEndian.PutUint16(m[4:6], 1)
	return append(m, []byte("\x03api\x06github\x03com\x00\x00\x01\x00\x01")...)
}

func TestUDPAndTCPForwarding(t *testing.T) {
	srv := fakeResolver(t)
	defer srv.Close()
	f := New(srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	udp := "127.0.0.1:0"
	pc, _ := net.ListenPacket("udp", udp)
	udp = pc.LocalAddr().String()
	pc.Close()
	go f.ServeUDP(ctx, udp)
	tl, _ := net.Listen("tcp", "127.0.0.1:0")
	tcp := tl.Addr().String()
	tl.Close()
	go f.ServeTCP(ctx, tcp)
	time.Sleep(100 * time.Millisecond)

	c, err := net.Dial("udp", udp)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(query(0xbeef))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || binary.BigEndian.Uint16(buf[:2]) != 0xbeef || buf[2]&0x80 == 0 || n < 12 {
		t.Fatalf("udp reply: n=%d err=%v", n, err)
	}

	tc, err := net.Dial("tcp", tcp)
	if err != nil {
		t.Fatal(err)
	}
	tc.SetDeadline(time.Now().Add(3 * time.Second))
	q := query(0x1234)
	binary.Write(tc, binary.BigEndian, uint16(len(q)))
	tc.Write(q)
	var l uint16
	binary.Read(tc, binary.BigEndian, &l)
	r := make([]byte, l)
	if _, err := io.ReadFull(tc, r); err != nil || binary.BigEndian.Uint16(r[:2]) != 0x1234 {
		t.Fatalf("tcp reply: %v", err)
	}
}

func TestServfailOnUpstreamError(t *testing.T) {
	f := New("http://127.0.0.1:1/dns-query", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := f.Exchange(context.Background(), query(1)); err == nil {
		t.Fatal("expected an error")
	}
	r := servfail(query(7))
	if r[3]&0x0f != 2 || r[2]&0x80 == 0 || binary.BigEndian.Uint16(r[:2]) != 7 {
		t.Fatalf("servfail = %x", r[:4])
	}
}
