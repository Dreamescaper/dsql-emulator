package proxy

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// relay pumps bytes between a client and its upstream connection until both
// directions have finished. It half-closes on clean EOF so a client that
// shuts down its write side can still read the server's response.
func relay(upstream, client net.Conn, fromClient, fromUpstream *atomic.Int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pipe(upstream, client, fromClient)
	}()
	go func() {
		defer wg.Done()
		pipe(client, upstream, fromUpstream)
	}()
	wg.Wait()
}

// pipe copies src into dst until EOF or error, recording the byte count.
func pipe(dst, src net.Conn, counter *atomic.Int64) {
	buf := make([]byte, relayBufferSize)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			counter.Add(int64(n))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				closePair(src, dst)
				return
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				closeWrite(dst)
			} else {
				closePair(src, dst)
			}
			return
		}
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func closePair(a, b net.Conn) {
	_ = a.Close()
	_ = b.Close()
}
