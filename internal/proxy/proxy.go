package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dreamescaper/dsql-emulator/internal/classify"
	"github.com/Dreamescaper/dsql-emulator/internal/txn"
	"github.com/Dreamescaper/dsql-emulator/rules"
)

const relayBufferSize = 32 * 1024

// Config configures the emulator proxy.
type Config struct {
	// Listen is the address the emulator accepts client connections on.
	Listen string
	// Upstream is the address of the backing PostgreSQL server.
	Upstream string
	// DialTimeout bounds how long an upstream dial may take.
	DialTimeout time.Duration
	// Logger receives structured proxy events. Defaults to slog.Default.
	Logger *slog.Logger
	// Classifier decides which statements are acceptable. Defaults to the
	// embedded Aurora DSQL ruleset.
	Classifier *classify.Classifier
}

// Proxy relays PostgreSQL wire-protocol traffic between clients and a backing
// PostgreSQL server. Every accepted client connection gets its own upstream
// connection, so backend session state stays pinned 1:1.
type Proxy struct {
	cfg        Config
	logger     *slog.Logger
	classifier *classify.Classifier

	ready   chan struct{}
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	listen  net.Listener
	accepts atomic.Int64
}

// New validates cfg and returns a Proxy ready to Run.
func New(cfg Config) (*Proxy, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:5432"
	}
	if cfg.Upstream == "" {
		return nil, errors.New("upstream address is required")
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Classifier == nil {
		rs, err := rules.Default()
		if err != nil {
			return nil, fmt.Errorf("load default ruleset: %w", err)
		}
		cfg.Classifier = classify.New(rs)
	}
	return &Proxy{
		cfg:        cfg,
		logger:     cfg.Logger,
		classifier: cfg.Classifier,
		ready:      make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}, nil
}

// Addr blocks until the proxy is listening and returns the bound address.
// It is safe to call from another goroutine while Run is in flight.
func (p *Proxy) Addr() string {
	<-p.ready
	return p.listen.Addr().String()
}

// Run listens for clients and relays them until ctx is cancelled.
func (p *Proxy) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", p.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", p.cfg.Listen, err)
	}
	p.listen = ln
	close(p.ready)

	stop := context.AfterFunc(ctx, func() {
		_ = ln.Close()
		p.closeConns()
	})
	defer stop()

	p.logger.Info("proxy listening", "listen", ln.Addr().String(), "upstream", p.cfg.Upstream)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		p.accepts.Add(1)
		go p.serve(ctx, conn)
	}
}

// serve pins one client connection to one upstream connection. It intercepts
// protocol traffic when it can and falls back to a raw relay otherwise.
func (p *Proxy) serve(ctx context.Context, client net.Conn) {
	defer client.Close()

	log := p.logger.With("client", client.RemoteAddr().String())

	upstream, err := p.dialUpstream(ctx)
	if err != nil {
		log.Warn("upstream dial failed", "upstream", p.cfg.Upstream, "err", err)
		return
	}
	defer upstream.Close()

	p.track(client)
	p.track(upstream)
	defer p.untrack(client)
	defer p.untrack(upstream)

	limits := p.classifier.Ruleset().Limits
	s := &session{
		logger:     log,
		client:     client,
		upstream:   upstream,
		classifier: p.classifier,
		tracker: txn.New(txn.Limits{
			DMLRows: limits.DMLRowsPerTxn,
			MaxAge:  time.Duration(limits.TxnAgeSeconds) * time.Second,
		}),
		statements: make(map[string][]classify.Kind),
		txStatus:   'I',
	}

	start := time.Now()
	intercept, err := s.handshake()
	switch {
	case err != nil:
		log.Debug("handshake ended", "err", err)
	case !intercept:
		relay(upstream, client, &s.fromClient, &s.fromUpstream)
	default:
		s.run()
	}

	log.Debug("connection closed",
		"duration", time.Since(start),
		"bytes_to_upstream", s.fromClient.Load(),
		"bytes_from_upstream", s.fromUpstream.Load(),
	)
}

func (p *Proxy) dialUpstream(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: p.cfg.DialTimeout}
	return d.DialContext(ctx, "tcp", p.cfg.Upstream)
}

func (p *Proxy) track(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[c] = struct{}{}
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, c)
}

func (p *Proxy) closeConns() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
}
