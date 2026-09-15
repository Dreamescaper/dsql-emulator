package proxy

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
)

func TestRelayEchoes(t *testing.T) {
	upstreamAddr := startEchoServer(t)
	upstream, err := net.Dial("tcp", upstreamAddr)
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	app, proxySide := tcpPair(t)

	var fromClient, fromUpstream atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay(upstream, proxySide, &fromClient, &fromUpstream)
	}()

	want := []byte("hello dsql\n")
	if _, err := app.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(app, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got %q want %q", got, want)
	}

	_ = app.Close()
	_ = upstream.Close()
	<-done
}

func TestRelayHalfCloseDeliversTrailingResponse(t *testing.T) {
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = upstreamLn.Close() })

	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		_, _ = conn.Write([]byte("done"))
	}()

	upstream, err := net.Dial("tcp", upstreamLn.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	app, proxySide := tcpPair(t)

	var fromClient, fromUpstream atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay(upstream, proxySide, &fromClient, &fromUpstream)
	}()

	appTCP, ok := app.(*net.TCPConn)
	if !ok {
		t.Fatalf("expected *net.TCPConn, got %T", app)
	}
	if _, err := appTCP.Write([]byte("request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := appTCP.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}

	got, err := io.ReadAll(app)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "done" {
		t.Fatalf("got %q want %q", got, "done")
	}

	_ = upstream.Close()
	<-done
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		conn, err := ln.Accept()
		ch <- accepted{conn, err}
	}()

	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	a := <-ch
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}
	t.Cleanup(func() {
		_ = dialed.Close()
		_ = a.conn.Close()
	})
	return dialed, a.conn
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
