//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestPipelinedClientIsNotDisturbed covers the shape that broke Npgsql on
// v0.2.0: a client that sends its next request before the previous one is
// answered. The emulator writes its own statements into the same stream, so
// anything injected after the fact lands behind the client's messages, and the
// exchange meant to hide the emulator's own answer hides the client's instead.
//
// The client must see exactly what the backend said, in order.
func TestPipelinedClientIsNotDisturbed(t *testing.T) {
	ctx := context.Background()
	dsn := startEmulator(t, ctx)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `CREATE TABLE pipe_t (id int PRIMARY KEY, v text)`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO pipe_t (id, v) VALUES (1, 'seed')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("a transaction and its first statement in one batch", func(t *testing.T) {
		pipeline := conn.PgConn().StartPipeline(ctx)
		pipeline.SendQueryParams("BEGIN", nil, nil, nil, nil)
		pipeline.SendQueryParams("UPDATE pipe_t SET v = 'pipelined' WHERE id = 1", nil, nil, nil, nil)
		pipeline.SendQueryParams("COMMIT", nil, nil, nil, nil)
		if err := pipeline.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}

		want := []string{"BEGIN", "UPDATE 1", "COMMIT"}
		for _, tag := range want {
			results, err := pipeline.GetResults()
			if err != nil {
				t.Fatalf("expecting %q: %v", tag, err)
			}
			reader, ok := results.(*pgconn.ResultReader)
			if !ok {
				t.Fatalf("expecting %q, got %T", tag, results)
			}
			result := reader.Read()
			if result.Err != nil {
				t.Fatalf("%q: %v", tag, result.Err)
			}
			if got := result.CommandTag.String(); got != tag {
				t.Fatalf("got command tag %q want %q", got, tag)
			}
		}
		if err := pipeline.Close(); err != nil {
			t.Fatalf("close pipeline: %v", err)
		}

		var v string
		if err := conn.QueryRow(ctx, `SELECT v FROM pipe_t WHERE id = 1`).Scan(&v); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if v != "pipelined" {
			t.Fatalf("got %q want %q; the pipelined update did not commit", v, "pipelined")
		}
	})

	// A conflict on the first statement of a pipelined transaction is still
	// adjudicated at COMMIT. The client leaves no room to establish a
	// savepoint, so the transaction is repaired by being started again, which
	// is sound while that statement is the only one it holds.
	t.Run("a conflict on a pipelined first statement defers to commit", func(t *testing.T) {
		holder, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer holder.Close(ctx)

		if _, err := holder.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("holder begin: %v", err)
		}
		if _, err := holder.Exec(ctx, `UPDATE pipe_t SET v = 'holder' WHERE id = 1`); err != nil {
			t.Fatalf("holder update: %v", err)
		}

		loser, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer loser.Close(ctx)

		// BEGIN and the update go out together, with no Sync between them.
		pipeline := loser.PgConn().StartPipeline(ctx)
		pipeline.SendQueryParams("BEGIN", nil, nil, nil, nil)
		pipeline.SendQueryParams(`UPDATE pipe_t SET v = 'loser' WHERE id = 1`, nil, nil, nil, nil)
		if err := pipeline.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}

		for _, tag := range []string{"BEGIN", "UPDATE 1"} {
			results, err := pipeline.GetResults()
			if err != nil {
				t.Fatalf("expecting %q: %v", tag, err)
			}
			reader, ok := results.(*pgconn.ResultReader)
			if !ok {
				t.Fatalf("expecting %q, got %T", tag, results)
			}
			result := reader.Read()
			if result.Err != nil {
				t.Fatalf("%q was refused instead of being deferred to commit: %v", tag, result.Err)
			}
			if got := result.CommandTag.String(); got != tag {
				t.Fatalf("got command tag %q want %q", got, tag)
			}
		}
		if err := pipeline.Close(); err != nil {
			t.Fatalf("close pipeline: %v", err)
		}

		if _, err := holder.Exec(ctx, "COMMIT"); err != nil {
			t.Fatalf("the first committer should succeed: %v", err)
		}

		_, err = loser.Exec(ctx, "COMMIT")
		assertSQLState(t, err, "40001")
	})

	// A backend error inside a pipelined transaction has to reach the client as
	// that error, rather than as a message it was not expecting.
	t.Run("an error inside a pipelined transaction", func(t *testing.T) {
		pipeline := conn.PgConn().StartPipeline(ctx)
		pipeline.SendQueryParams("BEGIN", nil, nil, nil, nil)
		pipeline.SendQueryParams(`SELECT 1 FROM "NoSuchTableXYZ"`, nil, nil, nil, nil)
		if err := pipeline.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}

		// The BEGIN succeeds.
		results, err := pipeline.GetResults()
		if err != nil {
			t.Fatalf("expecting BEGIN: %v", err)
		}
		if reader, ok := results.(*pgconn.ResultReader); ok {
			if result := reader.Read(); result.Err != nil {
				t.Fatalf("BEGIN: %v", result.Err)
			}
		}

		// The statement fails with the backend's own error.
		results, err = pipeline.GetResults()
		if err == nil {
			if reader, ok := results.(*pgconn.ResultReader); ok {
				err = reader.Read().Err
			}
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("got %T (%v), want the backend's error", err, err)
		}
		if pgErr.Code != "42P01" {
			t.Fatalf("got SQLSTATE %s want 42P01 (%s)", pgErr.Code, pgErr.Message)
		}
		_ = pipeline.Close()
	})
}
