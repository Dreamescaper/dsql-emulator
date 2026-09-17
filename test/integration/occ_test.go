//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Dreamescaper/dsql-emulator/internal/proxy"
)

// startEmulator brings up a backing database with the published init scripts
// and an emulator in front of it, and returns the DSN clients connect on.
func startEmulator(t *testing.T, ctx context.Context) string {
	t.Helper()

	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.WithInitScripts(initScripts(t)...),
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
	t.Cleanup(cancel)
	go func() { _ = p.Run(runCtx) }()

	return "postgres://postgres:postgres@" + p.Addr() + "/postgres?sslmode=disable"
}

// stepTimeout bounds one statement. Aurora DSQL never waits for a lock, so a
// statement that takes this long has blocked, which is the divergence the
// adjudicator exists to remove.
const stepTimeout = 3 * time.Second

// execStep runs one statement under a timeout and returns its command tag.
func execStep(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) (string, error) {
	t.Helper()
	stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	started := time.Now()
	tag, err := conn.Exec(stepCtx, sql)
	if elapsed := time.Since(started); elapsed >= stepTimeout {
		t.Fatalf("%q blocked for %s; Aurora DSQL never waits for a lock", sql, elapsed)
	}
	return tag.String(), err
}

// TestOccAdjudicatesConflictsAtCommit covers the conflict sources Aurora DSQL
// resolves at commit. In each case the second transaction's statement must
// succeed without waiting, and the transaction must fail at COMMIT.
func TestOccAdjudicatesConflictsAtCommit(t *testing.T) {
	ctx := context.Background()
	dsn := startEmulator(t, ctx)

	setup, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer setup.Close(ctx)

	for _, sql := range []string{
		`CREATE TABLE occ_parent (id int PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE occ_child (id int PRIMARY KEY, parent_id int NOT NULL REFERENCES occ_parent(id))`,
	} {
		if _, err := setup.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	tests := []struct {
		name string
		// winner runs first and takes the lock; loser must not wait for it.
		winner  string
		loser   string
		wantTag string
	}{
		{
			name:    "write against write",
			winner:  `UPDATE occ_parent SET name = 'a' WHERE id = %[1]d`,
			loser:   `UPDATE occ_parent SET name = 'b' WHERE id = %[1]d`,
			wantTag: "UPDATE 1",
		},
		{
			name:    "for update against write",
			winner:  `SELECT name FROM occ_parent WHERE id = %[1]d FOR UPDATE`,
			loser:   `UPDATE occ_parent SET name = 'b' WHERE id = %[1]d`,
			wantTag: "UPDATE 1",
		},
		{
			name:    "for key share against delete",
			winner:  `SELECT name FROM occ_parent WHERE id = %[1]d FOR KEY SHARE`,
			loser:   `DELETE FROM occ_parent WHERE id = %[1]d`,
			wantTag: "DELETE 1",
		},
		{
			name:    "write against for update",
			winner:  `UPDATE occ_parent SET name = 'a' WHERE id = %[1]d`,
			loser:   `SELECT name FROM occ_parent WHERE id = %[1]d FOR UPDATE`,
			wantTag: "SELECT 1",
		},
		{
			name:    "delete of a referenced row against a referencing insert",
			winner:  `DELETE FROM occ_parent WHERE id = %[1]d`,
			loser:   `INSERT INTO occ_child (id, parent_id) VALUES (%[1]d, %[1]d)`,
			wantTag: "INSERT 0 1",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := 100 + i
			if _, err := setup.Exec(ctx, `INSERT INTO occ_parent (id, name) VALUES ($1, 'seed')`, id); err != nil {
				t.Fatalf("seed row: %v", err)
			}

			a, b := connectPair(t, ctx, dsn)

			if _, err := execStep(t, ctx, a, "BEGIN"); err != nil {
				t.Fatalf("session a begin: %v", err)
			}
			if _, err := execStep(t, ctx, a, sqlf(tt.winner, id)); err != nil {
				t.Fatalf("session a statement: %v", err)
			}

			if _, err := execStep(t, ctx, b, "BEGIN"); err != nil {
				t.Fatalf("session b begin: %v", err)
			}
			// The losing statement succeeds, as it does on Aurora DSQL, and
			// reports what it would have done.
			tag, err := execStep(t, ctx, b, sqlf(tt.loser, id))
			if err != nil {
				t.Fatalf("session b statement failed instead of being deferred to commit: %v", err)
			}
			if tag != tt.wantTag {
				t.Errorf("got command tag %q want %q", tag, tt.wantTag)
			}

			if _, err := execStep(t, ctx, a, "COMMIT"); err != nil {
				t.Fatalf("the first committer should succeed: %v", err)
			}

			_, err = execStep(t, ctx, b, "COMMIT")
			assertSQLState(t, err, "40001")
			assertConflictMessage(t, err)
		})
	}
}

// TestOccLeavesDisjointWorkAlone covers the pairs Aurora DSQL does not treat as
// a conflict, which must still commit on both sides.
func TestOccLeavesDisjointWorkAlone(t *testing.T) {
	ctx := context.Background()
	dsn := startEmulator(t, ctx)

	setup, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer setup.Close(ctx)

	for _, sql := range []string{
		`CREATE TABLE occ_parent (id int PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE occ_child (id int PRIMARY KEY, parent_id int NOT NULL REFERENCES occ_parent(id))`,
		`INSERT INTO occ_parent (id, name) VALUES (1, 'a'), (2, 'b'), (3, 'c')`,
	} {
		if _, err := setup.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	tests := []struct {
		name string
		a    string
		b    string
	}{
		{
			name: "writes to different rows",
			a:    `UPDATE occ_parent SET name = 'a2' WHERE id = 1`,
			b:    `UPDATE occ_parent SET name = 'b2' WHERE id = 2`,
		},
		{
			name: "a non-key update against a referencing insert",
			a:    `UPDATE occ_parent SET name = 'c2' WHERE id = 3`,
			b:    `INSERT INTO occ_child (id, parent_id) VALUES (30, 3)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := connectPair(t, ctx, dsn)

			for _, step := range []struct {
				conn *pgx.Conn
				sql  string
			}{
				{a, "BEGIN"}, {a, tt.a}, {b, "BEGIN"}, {b, tt.b}, {a, "COMMIT"}, {b, "COMMIT"},
			} {
				if _, err := execStep(t, ctx, step.conn, step.sql); err != nil {
					t.Fatalf("%q: %v", step.sql, err)
				}
			}
		})
	}
}

// TestOccAdjudicatesParameterisedStatements covers the shape application code
// actually writes. The shadow that answers a refused statement is a different
// statement, so it keeps only the parameters it still refers to and is bound
// with the values the client bound for those.
func TestOccAdjudicatesParameterisedStatements(t *testing.T) {
	ctx := context.Background()
	dsn := startEmulator(t, ctx)

	setup, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer setup.Close(ctx)

	for _, sql := range []string{
		`CREATE TABLE occ_param (id int PRIMARY KEY, k int NOT NULL, name text NOT NULL)`,
		`INSERT INTO occ_param (id, k, name) VALUES (1, 7, 'a'), (2, 7, 'b'), (3, 7, 'c'), (4, 7, 'd')`,
	} {
		if _, err := setup.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	tests := []struct {
		name string
		// winner takes the lock; loser must be answered rather than made to wait.
		winner     string
		winnerArgs []any
		loser      string
		loserArgs  []any
		wantTag    string
		wantRows   int
	}{
		{
			name:       "the set list is dropped and the predicate is bound",
			winner:     `UPDATE occ_param SET name = $1 WHERE id = $2`,
			winnerArgs: []any{"w", 1},
			loser:      `UPDATE occ_param SET name = $1 WHERE id = $2`,
			loserArgs:  []any{"l", 1},
			wantTag:    "UPDATE 1",
		},
		{
			name:       "several predicate parameters are renumbered together",
			winner:     `UPDATE occ_param SET name = $1 WHERE id = $2 AND k = $3`,
			winnerArgs: []any{"w", 2, 7},
			loser:      `UPDATE occ_param SET name = $1 WHERE id = $2 AND k = $3`,
			loserArgs:  []any{"l", 2, 7},
			wantTag:    "UPDATE 1",
		},
		{
			name:       "a parameterised delete",
			winner:     `UPDATE occ_param SET name = $1 WHERE id = $2`,
			winnerArgs: []any{"w", 3},
			loser:      `DELETE FROM occ_param WHERE id = $1`,
			loserArgs:  []any{3},
			wantTag:    "DELETE 1",
		},
		{
			name:       "a parameterised locking select keeps its rows",
			winner:     `UPDATE occ_param SET name = $1 WHERE id = $2`,
			winnerArgs: []any{"w", 4},
			loser:      `SELECT name FROM occ_param WHERE id = $1 FOR UPDATE`,
			loserArgs:  []any{4},
			wantTag:    "SELECT 1",
			wantRows:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := connectPair(t, ctx, dsn)

			if _, err := execStep(t, ctx, a, "BEGIN"); err != nil {
				t.Fatalf("session a begin: %v", err)
			}
			if _, err := a.Exec(ctx, tt.winner, tt.winnerArgs...); err != nil {
				t.Fatalf("session a statement: %v", err)
			}

			if _, err := execStep(t, ctx, b, "BEGIN"); err != nil {
				t.Fatalf("session b begin: %v", err)
			}

			// The losing statement succeeds and reports what it would have done.
			started := time.Now()
			stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
			rows, err := b.Query(stepCtx, tt.loser, tt.loserArgs...)
			var tag string
			var scanned int
			if err == nil {
				for rows.Next() {
					scanned++
				}
				err = rows.Err()
				tag = rows.CommandTag().String()
				rows.Close()
			}
			cancel()
			if elapsed := time.Since(started); elapsed >= stepTimeout {
				t.Fatalf("the losing statement blocked for %s", elapsed)
			}
			if err != nil {
				t.Fatalf("session b statement failed instead of being deferred to commit: %v", err)
			}
			if tag != tt.wantTag {
				t.Errorf("got command tag %q want %q", tag, tt.wantTag)
			}
			if scanned != tt.wantRows {
				t.Errorf("got %d rows want %d", scanned, tt.wantRows)
			}

			if _, err := execStep(t, ctx, a, "COMMIT"); err != nil {
				t.Fatalf("the first committer should succeed: %v", err)
			}

			_, err = execStep(t, ctx, b, "COMMIT")
			assertSQLState(t, err, "40001")
			assertConflictMessage(t, err)
		})
	}
}

// A conflict outside an explicit transaction has no commit to be deferred to,
// so the statement itself reports it, with Aurora DSQL's wording.
func TestOccReportsConflictOnImplicitTransaction(t *testing.T) {
	ctx := context.Background()
	dsn := startEmulator(t, ctx)

	setup, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer setup.Close(ctx)

	for _, sql := range []string{
		`CREATE TABLE occ_solo (id int PRIMARY KEY, name text NOT NULL)`,
		`INSERT INTO occ_solo (id, name) VALUES (1, 'a')`,
	} {
		if _, err := setup.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	a, b := connectPair(t, ctx, dsn)
	if _, err := execStep(t, ctx, a, "BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := execStep(t, ctx, a, `UPDATE occ_solo SET name = 'held' WHERE id = 1`); err != nil {
		t.Fatalf("hold the row: %v", err)
	}

	_, err = execStep(t, ctx, b, `UPDATE occ_solo SET name = 'other' WHERE id = 1`)
	assertSQLState(t, err, "40001")
	assertConflictMessage(t, err)
}

// assertConflictMessage checks the error carries Aurora DSQL's wording, not
// PostgreSQL's.
func assertConflictMessage(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "change conflicts with another transaction (OC000)") {
		t.Fatalf("got %v, want Aurora DSQL's conflict wording", err)
	}
}

// sqlf fills a case's row id into its statement.
func sqlf(format string, id int) string { return fmt.Sprintf(format, id) }

func connectPair(t *testing.T, ctx context.Context, dsn string) (*pgx.Conn, *pgx.Conn) {
	t.Helper()
	conns := make([]*pgx.Conn, 2)
	for i := range conns {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect session %d: %v", i, err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		conns[i] = conn
	}
	return conns[0], conns[1]
}
