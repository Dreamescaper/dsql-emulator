//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Dreamescaper/dsql-emulator/internal/proxy"
)

// initScripts returns every backing-database init script, so the tests apply
// the same setup as the published image.
func initScripts(t *testing.T) []string {
	t.Helper()
	scripts, err := filepath.Glob("../../docker/init/*.sql")
	if err != nil || len(scripts) == 0 {
		t.Fatalf("find init scripts: %v (%d found)", err, len(scripts))
	}
	return scripts
}

func TestPgxRoundTripThroughProxy(t *testing.T) {
	ctx := context.Background()

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

	tlsConfig, err := proxy.SelfSignedTLSConfig("localhost")
	if err != nil {
		t.Fatalf("tls config: %v", err)
	}

	p, err := proxy.New(proxy.Config{
		Listen:   "127.0.0.1:0",
		Upstream: net.JoinHostPort(host, port.Port()),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		TLS:      tlsConfig,
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
		assertSQLState(t, err, "42704")
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

	t.Run("tls connection is intercepted", func(t *testing.T) {
		tlsConn, err := pgx.Connect(ctx, "postgres://postgres:postgres@"+p.Addr()+"/postgres?sslmode=require")
		if err != nil {
			t.Fatalf("connect with sslmode=require: %v", err)
		}
		defer tlsConn.Close(context.Background())

		var got int
		if err := tlsConn.QueryRow(ctx, "select 1").Scan(&got); err != nil {
			t.Fatalf("query over tls: %v", err)
		}
		if got != 1 {
			t.Fatalf("got %d want 1", got)
		}

		// Interception must survive TLS: an unsupported statement is still refused.
		_, err = tlsConn.Exec(ctx, "TRUNCATE widget")
		assertSQLState(t, err, "0A000")
	})

	t.Run("reports the dsql server version", func(t *testing.T) {
		if got := conn.PgConn().ParameterStatus("server_version"); got != proxy.DefaultServerVersion {
			t.Fatalf("parameter server_version %q want %q", got, proxy.DefaultServerVersion)
		}

		var version string
		if err := conn.QueryRow(ctx, "select version()").Scan(&version); err != nil {
			t.Fatalf("select version(): %v", err)
		}
		if version != proxy.DefaultVersionFunction {
			t.Fatalf("got %q want %q", version, proxy.DefaultVersionFunction)
		}

		var showVersion string
		if err := conn.QueryRow(ctx, "show server_version").Scan(&showVersion); err != nil {
			t.Fatalf("show server_version: %v", err)
		}
		if showVersion != proxy.DefaultServerVersion {
			t.Fatalf("got server_version %q want %q", showVersion, proxy.DefaultServerVersion)
		}
	})

	t.Run("async index records a job", func(t *testing.T) {
		var jobID string
		if err := conn.QueryRow(ctx, "CREATE INDEX ASYNC IF NOT EXISTS widget_async_idx ON widget (name)").Scan(&jobID); err != nil {
			t.Fatalf("create index async: %v", err)
		}
		if jobID == "" {
			t.Fatal("expected a job id")
		}

		// The id handed to the client has to be findable in sys.jobs, with the
		// shape Aurora DSQL reports.
		var jobType, status, objectName string
		if err := conn.QueryRow(ctx,
			"SELECT job_type, status, object_name FROM sys.jobs WHERE job_id = $1", jobID).
			Scan(&jobType, &status, &objectName); err != nil {
			t.Fatalf("the returned job id is not in sys.jobs: %v", err)
		}
		if jobType != "INDEX_BUILD" || status != "completed" {
			t.Fatalf("got job_type %q status %q want INDEX_BUILD/completed", jobType, status)
		}
		if objectName != "public.widget_async_idx" {
			t.Fatalf("got object_name %q want public.widget_async_idx", objectName)
		}

		// wait_for_job is a procedure, so CALL is how it is used, and a SELECT
		// against it fails the way it does on DSQL.
		if _, err := conn.Exec(ctx, "CALL sys.wait_for_job($1)", jobID); err != nil {
			t.Fatalf("call wait_for_job: %v", err)
		}
		// The same call with a literal, which is how it reads without a job
		// lying around, still resolves the procedure.
		_, err = conn.Exec(ctx, "CALL sys.wait_for_job('no-such-job')")
		assertSQLState(t, err, "22P02")
		var ignored string
		err := conn.QueryRow(ctx, "SELECT sys.wait_for_job($1)", jobID).Scan(&ignored)
		assertSQLState(t, err, "42809")

		// The index really exists.
		if _, err := conn.Exec(ctx, "drop index widget_async_idx"); err != nil {
			t.Fatalf("the index was not created: %v", err)
		}
	})

	t.Run("partial index is created and recorded", func(t *testing.T) {
		var jobID string
		if err := conn.QueryRow(ctx,
			"CREATE INDEX ASYNC widget_partial_idx ON widget (name) WHERE name <> ''").Scan(&jobID); err != nil {
			t.Fatalf("create partial index: %v", err)
		}

		// The predicate has to survive the rewrite, or it is not a partial index.
		var definition string
		if err := conn.QueryRow(ctx,
			"SELECT indexdef FROM pg_indexes WHERE indexname = 'widget_partial_idx'").Scan(&definition); err != nil {
			t.Fatalf("the partial index was not created: %v", err)
		}
		if !strings.Contains(definition, "WHERE") {
			t.Fatalf("index definition %q carries no predicate", definition)
		}

		var status string
		if err := conn.QueryRow(ctx, "SELECT status FROM sys.jobs WHERE job_id = $1", jobID).Scan(&status); err != nil {
			t.Fatalf("the returned job id is not in sys.jobs: %v", err)
		}
		if status != "completed" {
			t.Fatalf("got status %q want completed", status)
		}

		if _, err := conn.Exec(ctx, "drop index widget_partial_idx"); err != nil {
			t.Fatalf("drop index: %v", err)
		}
	})

	t.Run("constraint validation is async and recorded", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "create table if not exists alter_parent (id int primary key)"); err != nil {
			t.Fatalf("create referenced table: %v", err)
		}
		if _, err := conn.Exec(ctx, "create table if not exists alter_fk (id int primary key, parent_id int)"); err != nil {
			t.Fatalf("create table: %v", err)
		}
		// A constraint added by ALTER TABLE has to use NOT VALID.
		_, err := conn.Exec(ctx, "alter table alter_fk add constraint alter_fk_parent foreign key (parent_id) references alter_parent(id)")
		assertSQLState(t, err, "0A000")

		if _, err := conn.Exec(ctx, "alter table alter_fk add constraint alter_fk_parent foreign key (parent_id) references alter_parent(id) not valid"); err != nil {
			t.Fatalf("add not valid constraint: %v", err)
		}

		// Validation only takes the ASYNC form, and returns a job id.
		var jobID string
		if err := conn.QueryRow(ctx, "alter table async alter_fk validate constraint alter_fk_parent").Scan(&jobID); err != nil {
			t.Fatalf("validate constraint: %v", err)
		}
		var jobType, status string
		if err := conn.QueryRow(ctx, "select job_type, status from sys.jobs where job_id = $1", jobID).
			Scan(&jobType, &status); err != nil {
			t.Fatalf("the returned job id is not in sys.jobs: %v", err)
		}
		if status != "completed" {
			t.Fatalf("got status %q want completed", status)
		}
		if _, err := conn.Exec(ctx, "call sys.wait_for_job($1)", jobID); err != nil {
			t.Fatalf("call wait_for_job: %v", err)
		}

		// Without ASYNC it is refused.
		_, err = conn.Exec(ctx, "alter table alter_fk validate constraint alter_fk_parent")
		assertSQLState(t, err, "0A000")
	})

	t.Run("dropping a primary key column is not refused yet", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "create table if not exists alter_pk (id int primary key, a text)"); err != nil {
			t.Fatalf("create table: %v", err)
		}
		// Known gap: DSQL refuses this, the emulator does not.
		if _, err := conn.Exec(ctx, "alter table alter_pk drop column id"); err != nil {
			t.Fatalf("emulator refused a primary key column drop: %v", err)
		}
	})

	t.Run("qualified index name is refused", func(t *testing.T) {
		_, err := conn.Exec(ctx, "CREATE INDEX ASYNC public.widget_qualified_idx ON widget (name)")
		assertSQLState(t, err, "42601")
	})

	t.Run("synchronous index is still refused", func(t *testing.T) {
		_, err := conn.Exec(ctx, "CREATE INDEX widget_sync_idx ON widget (name)")
		assertSQLState(t, err, "0A000")
	})

	t.Run("concurrent updates conflict with an occ code", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "create table if not exists occ_t (id int primary key, v int)"); err != nil {
			t.Fatalf("create table: %v", err)
		}
		if _, err := conn.Exec(ctx, "insert into occ_t values (1, 0) on conflict (id) do nothing"); err != nil {
			t.Fatalf("seed row: %v", err)
		}

		second, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("second connection: %v", err)
		}
		defer second.Close(context.Background())

		first, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin first: %v", err)
		}
		if _, err := first.Exec(ctx, "update occ_t set v = v + 1 where id = 1"); err != nil {
			t.Fatalf("first update: %v", err)
		}

		// The second update blocks on PostgreSQL's row lock, which DSQL would
		// not do; once the first commits it fails as a serialization error.
		done := make(chan error, 1)
		go func() {
			other, err := second.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			if _, err := other.Exec(ctx, "update occ_t set v = v + 1 where id = 1"); err != nil {
				done <- err
				return
			}
			done <- other.Commit(ctx)
		}()

		time.Sleep(300 * time.Millisecond)
		if err := first.Commit(ctx); err != nil {
			t.Fatalf("first commit: %v", err)
		}

		err = <-done
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("expected a serialization error, got %v", err)
		}
		if pgErr.Code != "40001" {
			t.Fatalf("got SQLSTATE %s want 40001", pgErr.Code)
		}
		if !strings.Contains(pgErr.Message, "OC000") {
			t.Fatalf("message %q does not carry the OCC code", pgErr.Message)
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

	t.Run("isolation is repeatable read", func(t *testing.T) {
		var level string
		if err := conn.QueryRow(ctx, "show transaction_isolation").Scan(&level); err != nil {
			t.Fatalf("show transaction_isolation: %v", err)
		}
		if level != "repeatable read" {
			t.Fatalf("got %q want %q", level, "repeatable read")
		}
	})

	t.Run("rejects unsupported isolation level", func(t *testing.T) {
		_, err := conn.Exec(ctx, "BEGIN ISOLATION LEVEL SERIALIZABLE")
		assertSQLState(t, err, "0A000")
	})

	t.Run("rejects second DDL in a transaction", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := conn.Exec(ctx, "create table if not exists txn_ddl_a (id int)"); err != nil {
			t.Fatalf("first ddl: %v", err)
		}
		_, err := conn.Exec(ctx, "create table if not exists txn_ddl_b (id int)")
		assertSQLState(t, err, "0A000")

		if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	})

	t.Run("rejects DDL and DML in one transaction", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := conn.Exec(ctx, "create table if not exists txn_mix (id int)"); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		_, err := conn.Exec(ctx, "insert into txn_mix values (1)")
		assertSQLState(t, err, "0A000")

		if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	})

	t.Run("allows DDL and DML in separate transactions", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "create table if not exists txn_sep (id int)"); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		if _, err := conn.Exec(ctx, "insert into txn_sep values (1)"); err != nil {
			t.Fatalf("dml: %v", err)
		}
	})

	t.Run("enforces the row cap and permits rollback", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "create table if not exists txn_bulk (id int)"); err != nil {
			t.Fatalf("create table: %v", err)
		}
		if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("begin: %v", err)
		}

		// The statement that crosses the cap fails, and the transaction is left
		// aborted until the client ends it.
		_, err := conn.Exec(ctx, "insert into txn_bulk select generate_series(1, 3001)")
		assertSQLState(t, err, "54000")

		_, err = conn.Exec(ctx, "select 1")
		assertSQLState(t, err, "25P02")

		if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		var count int
		if err := conn.QueryRow(ctx, "select count(*) from txn_bulk").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("rollback left %d rows behind", count)
		}
	})

	t.Run("row cap fails an autocommit statement", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "create table if not exists txn_implicit (id int)"); err != nil {
			t.Fatalf("create table: %v", err)
		}

		_, err := conn.Exec(ctx, "insert into txn_implicit select generate_series(1, 3001)")
		assertSQLState(t, err, "54000")

		var count int
		if err := conn.QueryRow(ctx, "select count(*) from txn_implicit").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("committed %d rows from a statement that crossed the cap", count)
		}
	})

	t.Run("rejection aborts the transaction", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, err := conn.Exec(ctx, "TRUNCATE txn_bulk")
		assertSQLState(t, err, "0A000")

		_, err = conn.Exec(ctx, "select 1")
		assertSQLState(t, err, "25P02")

		if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
			t.Fatalf("commit on an aborted transaction should succeed: %v", err)
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

// TestTokenAuthThroughProxy checks the DSQL-shaped setup: TLS is required and
// the password is an opaque token that the backing database, on trust, accepts.
func TestTokenAuthThroughProxy(t *testing.T) {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithInitScripts(initScripts(t)...),
		testcontainers.WithEnv(map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"}),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	tlsConfig, err := proxy.SelfSignedTLSConfig("localhost")
	if err != nil {
		t.Fatalf("tls config: %v", err)
	}
	p, err := proxy.New(proxy.Config{
		Listen:   "127.0.0.1:0",
		Upstream: net.JoinHostPort(host, port.Port()),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		TLS:      tlsConfig,
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = p.Run(runCtx) }()

	// A token that is not a real password still connects.
	dsn := "postgres://postgres:an-iam-token-not-a-password@" + p.Addr() + "/postgres?sslmode=require"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect with a token: %v", err)
	}
	defer conn.Close(context.Background())

	var one int
	if err := conn.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	if one != 1 {
		t.Fatalf("got %d want 1", one)
	}
}
