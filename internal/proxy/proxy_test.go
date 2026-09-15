package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/Dreamescaper/dsql-emulator/internal/proxy"
)

func TestProxyRelaysBidirectional(t *testing.T) {
	upstream := startEchoServer(t)

	p := newTestProxy(t, upstream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	want := []byte("hello dsql\n")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got %q want %q", got, want)
	}
}

func TestProxyHalfCloseDeliversTrailingResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn) // wait for client half-close
		_, _ = conn.Write([]byte("done"))
	}()

	p := newTestProxy(t, ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("expected *net.TCPConn, got %T", conn)
	}
	if _, err := tcp.Write([]byte("request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "done" {
		t.Fatalf("got %q want %q", got, "done")
	}
}

func TestRunReturnsOnContextCancel(t *testing.T) {
	p := newTestProxy(t, "127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	_ = p.Addr()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func newTestProxy(t *testing.T, upstream string) *proxy.Proxy {
	t.Helper()
	p, err := proxy.New(proxy.Config{
		Listen:   "127.0.0.1:0",
		Upstream: upstream,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return p
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}
