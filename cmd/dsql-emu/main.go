package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Dreamescaper/dsql-emulator/internal/proxy"
)

func main() {
	var (
		listen   = flag.String("listen", "127.0.0.1:5432", "address to accept PostgreSQL clients on")
		upstream = flag.String("upstream", "127.0.0.1:5433", "address of the backing PostgreSQL server")
		logLevel = flag.String("log-level", "info", "log level: debug, info, warn, error")
	)
	flag.Parse()

	logger := newLogger(*logLevel)

	p, err := proxy.New(proxy.Config{
		Listen:   *listen,
		Upstream: *upstream,
		Logger:   logger,
	})
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := p.Run(ctx); err != nil {
		logger.Error("proxy stopped", "err", err)
		os.Exit(1)
	}
	logger.Info("proxy stopped")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		fmt.Fprintf(os.Stderr, "unknown log level %q, defaulting to info\n", level)
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
