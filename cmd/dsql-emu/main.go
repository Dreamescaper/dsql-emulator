package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Dreamescaper/dsql-emulator/internal/proxy"
)

func main() {
	var (
		listen        = flag.String("listen", "127.0.0.1:5432", "address to accept PostgreSQL clients on")
		upstream      = flag.String("upstream", "127.0.0.1:5433", "address of the backing PostgreSQL server")
		logLevel      = flag.String("log-level", "info", "log level: debug, info, warn, error")
		tlsCert       = flag.String("tls-cert", "", "PEM certificate for client TLS; a self-signed one is generated when unset")
		tlsKey        = flag.String("tls-key", "", "PEM private key for client TLS")
		noTLS         = flag.Bool("no-tls", false, "decline client TLS and stay plaintext")
		serverVersion = flag.String("server-version", proxy.DefaultServerVersion, "server_version reported to clients")
	)
	flag.Parse()

	logger := newLogger(*logLevel)

	tlsConfig, err := buildTLS(*noTLS, *tlsCert, *tlsKey, *listen)
	if err != nil {
		logger.Error("invalid TLS configuration", "err", err)
		os.Exit(2)
	}

	p, err := proxy.New(proxy.Config{
		Listen:        *listen,
		Upstream:      *upstream,
		Logger:        logger,
		TLS:           tlsConfig,
		ServerVersion: *serverVersion,
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

// buildTLS returns the TLS configuration for client connections, or nil when
// TLS is disabled.
func buildTLS(disabled bool, certFile, keyFile, listen string) (*tls.Config, error) {
	if disabled {
		return nil, nil
	}
	if certFile != "" || keyFile != "" {
		return proxy.LoadTLSConfig(certFile, keyFile)
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return proxy.SelfSignedTLSConfig(host)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		fmt.Fprintf(os.Stderr, "unknown log level %q, defaulting to info\n", level)
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
