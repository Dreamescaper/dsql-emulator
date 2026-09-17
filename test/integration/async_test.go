//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// simpleQuery runs a string the way `psql -c` does: one simple query, which may
// hold several statements, with every statement's result readable. pgx's own
// Query surfaces only the first, so it goes a level below it.
func simpleQuery(ctx context.Context, conn *pgx.Conn, sql string) ([]*pgconn.Result, error) {
	return conn.PgConn().Exec(ctx, sql).ReadAll()
}

// TestMultiStatementAsyncQuery drives the path `psql -c 'a; b'` takes: one
// simple query holding several statements, one of them spelled with ASYNC.
// PostgreSQL's parser has never heard of the keyword, so without the rewrite it
// answers with a syntax error where Aurora DSQL answers with its own rule.
func TestMultiStatementAsyncQuery(t *testing.T) {
	ctx := context.Background()
	dsn := startEmulator(t, ctx)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `CREATE TABLE async_t (id int PRIMARY KEY, a int)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Run("two DDL in one query are refused by the dialect", func(t *testing.T) {
		_, err := simpleQuery(ctx, conn, `CREATE TABLE async_u (id int); CREATE INDEX ASYNC i ON async_u (id)`)
		assertSQLState(t, err, "0A000")
	})

	t.Run("DDL mixed with DML is refused by the dialect", func(t *testing.T) {
		_, err := simpleQuery(ctx, conn, `INSERT INTO async_t (id, a) VALUES (1, 1); CREATE INDEX ASYNC i ON async_t (a)`)
		assertSQLState(t, err, "0A000")
	})

	t.Run("one DDL in a transaction runs and reports its job id", func(t *testing.T) {
		results, err := simpleQuery(ctx, conn, `BEGIN; CREATE INDEX ASYNC async_idx ON async_t (a); COMMIT`)
		if err != nil {
			t.Fatalf("the query should be accepted: %v", err)
		}

		// One result per statement, and the job id rides on the index build's
		// own, not on the BEGIN's.
		if len(results) != 3 {
			t.Fatalf("got %d results want 3 (BEGIN, CREATE INDEX, COMMIT)", len(results))
		}
		if len(results[0].Rows) != 0 {
			t.Errorf("the BEGIN carried %d rows, want none", len(results[0].Rows))
		}
		if len(results[2].Rows) != 0 {
			t.Errorf("the COMMIT carried %d rows, want none", len(results[2].Rows))
		}
		if len(results[1].Rows) != 1 || len(results[1].Rows[0]) != 1 {
			t.Fatalf("the index build carried %v, want one job id", results[1].Rows)
		}
		jobID := string(results[1].Rows[0][0])
		if len(jobID) != 36 {
			t.Fatalf("got job id %q, want a UUID", jobID)
		}

		// The index really was built, and the job recorded under that id.
		var count int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE indexname = 'async_idx'`).Scan(&count); err != nil {
			t.Fatalf("look up the index: %v", err)
		}
		if count != 1 {
			t.Fatalf("the index was not built (%d rows in pg_indexes)", count)
		}

		var status string
		if err := conn.QueryRow(ctx, `SELECT status FROM sys.jobs WHERE job_id = $1`, jobID).Scan(&status); err != nil {
			t.Fatalf("look up the job %s: %v", jobID, err)
		}
		if status != "completed" {
			t.Fatalf("got job status %q want completed", status)
		}
	})
}
