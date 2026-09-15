//go:build integration

package conformance_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Dreamescaper/dsql-emulator/internal/conformance"
	"github.com/Dreamescaper/dsql-emulator/internal/proxy"
)

const defaultGoldenDir = "golden"

// TestConformanceAgainstEmulator validates the harness itself and, when a
// golden record is present, checks the emulator against real DSQL behavior.
func TestConformanceAgainstEmulator(t *testing.T) {
	ctx := context.Background()
	conn := startEmulator(t, ctx)
	suite := conformance.DefaultSuite()

	var emulated *conformance.Golden
	t.Run("records every step and cleans up", func(t *testing.T) {
		golden, err := conformance.RunSuite(ctx, conn, suite, t.Logf)
		if err != nil {
			t.Fatalf("run suite: %v", err)
		}
		emulated = golden

		if len(golden.Cases) != len(suite.Cases) {
			t.Fatalf("recorded %d cases, expected %d", len(golden.Cases), len(suite.Cases))
		}
		for _, c := range golden.Cases {
			if len(c.Observations) != len(c.Steps) {
				t.Fatalf("case %q recorded %d observations for %d steps", c.Name, len(c.Observations), len(c.Steps))
			}
		}

		assertNoLeftovers(t, ctx, conn)
	})

	t.Run("matches the golden record", func(t *testing.T) {
		if emulated == nil {
			t.Skip("suite did not run")
		}

		dir := os.Getenv("GOLDEN_DIR")
		if dir == "" {
			dir = defaultGoldenDir
		}
		if _, err := os.Stat(dir); err != nil {
			t.Skipf("no golden fixtures in %s; run `make baseline` against a cluster first", dir)
		}

		golden, err := conformance.LoadDir(dir)
		if err != nil {
			t.Fatalf("load golden fixtures: %v", err)
		}

		diffs := conformance.CompareSuites(golden, emulated)
		for _, d := range diffs {
			if d.Advisory {
				t.Logf("note      %s", d)
			}
		}

		var known []conformance.Difference
		for _, d := range diffs {
			if d.KnownGap != "" && !d.Advisory {
				known = append(known, d)
			}
		}
		for _, d := range known {
			t.Logf("known gap %s", d)
		}

		if unrecorded := conformance.Unrecorded(golden, emulated); len(unrecorded) > 0 {
			t.Logf("%d case(s) not covered by the golden record; re-run `make baseline`: %v", len(unrecorded), unrecorded)
		}

		failures := conformance.Failures(diffs)
		if len(failures) == 0 {
			t.Logf("%d cases match the golden record", len(golden.Cases))
			return
		}

		report := fmt.Sprintf("%d difference(s) from the golden record:", len(failures))
		for _, d := range failures {
			report += "\n  " + d.String()
		}
		t.Fatal(report)
	})
}

func startEmulator(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()

	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.WithInitScripts("../../docker/init/01-sys.sql"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	p, err := proxy.New(proxy.Config{
		Listen:   "127.0.0.1:0",
		Upstream: net.JoinHostPort(host, port.Port()),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = p.Run(runCtx) }()

	dsn := "postgres://postgres:postgres@" + p.Addr() + "/postgres?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect through emulator: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func assertNoLeftovers(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()

	checks := []struct {
		name  string
		query string
	}{
		{"tables", `SELECT count(*) FROM information_schema.tables WHERE table_name LIKE 'baseline\_%'`},
		{"sequences", `SELECT count(*) FROM information_schema.sequences WHERE sequence_name LIKE 'baseline\_%'`},
		{"types", `SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE t.typname LIKE 'baseline\_%'`},
		{"schemas", `SELECT count(*) FROM information_schema.schemata WHERE schema_name = 'baseline_schema'`},
	}

	for _, check := range checks {
		var count int
		if err := conn.QueryRow(ctx, check.query).Scan(&count); err != nil {
			t.Fatalf("count leftover %s: %v", check.name, err)
		}
		if count != 0 {
			t.Fatalf("cleanup left %d %s behind", count, check.name)
		}
	}
}
