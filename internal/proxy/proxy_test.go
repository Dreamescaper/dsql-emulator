package proxy_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Dreamescaper/dsql-emulator/internal/proxy"
)

func TestRunReturnsOnContextCancel(t *testing.T) {
	p, err := proxy.New(proxy.Config{
		Listen:   "127.0.0.1:0",
		Upstream: "127.0.0.1:1",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

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

func TestNewRequiresUpstream(t *testing.T) {
	if _, err := proxy.New(proxy.Config{Listen: "127.0.0.1:0"}); err == nil {
		t.Fatal("expected error when upstream is empty")
	}
}
