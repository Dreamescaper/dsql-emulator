//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Dreamescaper/dsql-emulator/internal/proxy"
)

func TestPgxRoundTripThroughProxy(t *testing.T) {
	ctx := context.Background()

	pg, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
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
		if err := pg.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres: %v", err)
		}
	})

	host, err := pg.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := pg.MappedPort(ctx, "5432/tcp")
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
	defer cancel()
	go func() { _ = p.Run(runCtx) }()

	dsn := "postgres://postgres:postgres@" + p.Addr() + "/postgres?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect through proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	t.Run("simple query", func(t *testing.T) {
		var got int
		if err := conn.QueryRow(ctx, "select 1").Scan(&got); err != nil {
			t.Fatalf("query: %v", err)
		}
		if got != 1 {
			t.Fatalf("got %d want 1", got)
		}
	})

	t.Run("extended protocol with params", func(t *testing.T) {
		var got int
		if err := conn.QueryRow(ctx, "select $1::int + $2::int", 20, 22).Scan(&got); err != nil {
			t.Fatalf("query: %v", err)
		}
		if got != 42 {
			t.Fatalf("got %d want 42", got)
		}
	})

	t.Run("ddl and dml round trip", func(t *testing.T) {
		_, err := conn.Exec(ctx, `create table if not exists widget (id int primary key, name text not null)`)
		if err != nil {
			t.Fatalf("create table: %v", err)
		}
		_, err = conn.Exec(ctx, `insert into widget (id, name) values ($1, $2) on conflict (id) do nothing`, 1, "gizmo")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		var name string
		if err := conn.QueryRow(ctx, `select name from widget where id = $1`, 1).Scan(&name); err != nil {
			t.Fatalf("select: %v", err)
		}
		if name != "gizmo" {
			t.Fatalf("got %q want %q", name, "gizmo")
		}
	})

	t.Run("explicit transaction", func(t *testing.T) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `insert into widget (id, name) values ($1, $2) on conflict (id) do nothing`, 2, "sprocket"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var count int
		if err := conn.QueryRow(ctx, `select count(*) from widget`).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 2 {
			t.Fatalf("got %d rows want 2", count)
		}
	})

	t.Run("rejects unsupported simple protocol statement", func(t *testing.T) {
		_, err := conn.Exec(ctx, "TRUNCATE widget", pgx.QueryExecModeSimpleProtocol)
		assertSQLState(t, err, "0A000")
	})

	t.Run("rejects unsupported extended protocol statement", func(t *testing.T) {
		_, err := conn.Exec(ctx, "CREATE TRIGGER tr AFTER INSERT ON widget EXECUTE FUNCTION f()")
		assertSQLState(t, err, "0A000")
	})

	t.Run("rejects serial column", func(t *testing.T) {
		_, err := conn.Exec(ctx, "CREATE TABLE rejected_serial (id serial PRIMARY KEY)")
		assertSQLState(t, err, "0A000")
	})

	t.Run("connection usable after rejection", func(t *testing.T) {
		var got int
		if err := conn.QueryRow(ctx, "select 1").Scan(&got); err != nil {
			t.Fatalf("query after rejection: %v", err)
		}
		if got != 1 {
			t.Fatalf("got %d want 1", got)
		}
	})

	t.Run("foreign keys are supported", func(t *testing.T) {
		if _, err := conn.Exec(ctx, `create table if not exists parent (id int primary key)`); err != nil {
			t.Fatalf("create parent: %v", err)
		}
		if _, err := conn.Exec(ctx, `create table if not exists child (id int primary key, parent_id int references parent(id))`); err != nil {
			t.Fatalf("create child with foreign key: %v", err)
		}
	})
}

func assertSQLState(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got nil error", want)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != want {
		t.Fatalf("got SQLSTATE %s want %s (%s)", pgErr.Code, want, pgErr.Message)
	}
}
