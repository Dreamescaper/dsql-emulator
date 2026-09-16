// Command dsql-baseline records how a real Aurora DSQL cluster answers a suite
// of probes, producing a golden record that the emulator is checked against.
//
// The suite only touches objects prefixed with "baseline_" and drops everything
// it creates, including when a run fails, so it is safe to re-run.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"

	"github.com/Dreamescaper/dsql-emulator/internal/conformance"
)

func main() {
	var (
		host      = flag.String("host", "", "cluster endpoint host (required)")
		port      = flag.Int("port", 5432, "cluster endpoint port")
		user      = flag.String("user", "admin", "database user")
		database  = flag.String("database", "postgres", "database name")
		token     = flag.String("token", os.Getenv("DSQL_TOKEN"), "IAM auth token (or set DSQL_TOKEN)")
		tokenFile = flag.String("token-file", "", "read the IAM auth token from a file instead of --token")
		sslmode   = flag.String("sslmode", "require", "sslmode: require for DSQL, disable for a local server")
		outDir    = flag.String("out-dir", "test/conformance/golden", "directory to write one golden fixture per case group")
		label     = flag.String("label", "aurora-dsql", "target label stored in the record")
		dryRun    = flag.Bool("dry-run", false, "print the suite without connecting")
		cleanup   = flag.Bool("cleanup-only", false, "drop the suite's objects and exit")
	)
	flag.Parse()

	suite := conformance.DefaultSuite()

	if *dryRun {
		printSuite(suite)
		return
	}

	if *host == "" {
		fatal("--host is required")
	}

	tokenValue := *token
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			fatal("read token file: %v", err)
		}
		tokenValue = strings.TrimSpace(string(data))
	}
	if tokenValue == "" {
		fatal("provide --token, --token-file, or DSQL_TOKEN (generate one with `aws dsql generate-db-connect-auth-token`)")
	}

	printSuite(suite)
	fmt.Printf("\nAbout to run %d statements against %s (plus setup and cleanup).\n", statementCount(suite), *host)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := pgx.Connect(ctx, dsn(*user, tokenValue, *host, *port, *database, *sslmode))
	if err != nil {
		fatal("connect: %v", err)
	}
	defer conn.Close(context.Background())

	if *cleanup {
		for _, sql := range suite.Cleanup {
			if _, err := conn.Exec(ctx, sql); err != nil {
				fmt.Printf("cleanup skipped %q: %v\n", sql, err)
			}
		}
		if left := conformance.Leftovers(ctx, conn); len(left) > 0 {
			fmt.Printf("cleanup complete, but leftovers remain: %v\n", left)
			os.Exit(1)
		}
		fmt.Println("cleanup complete, no baseline_ objects remain")
		return
	}

	connect := func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.Connect(ctx, dsn(*user, tokenValue, *host, *port, *database, *sslmode))
	}
	golden, err := conformance.RunSuite(ctx, connect, suite, conformance.Options{
		IncludeRecordOnly: true,
		Progress: func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		},
	})
	if err != nil {
		// A partial run would record fewer cases than the suite has, so saving
		// it would replace a complete record with an incomplete one. Recording
		// costs a run against a real cluster, so leave what is on disk alone.
		fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "nothing written; the record in %s was left as it was\n", *outDir)
		os.Exit(1)
	}

	golden.Target = *label
	if err := conformance.SaveDir(*outDir, golden); err != nil {
		fatal("save: %v", err)
	}
	fmt.Printf("\nWrote %d cases to %s (one fixture per group)\n", len(golden.Cases), *outDir)
}

func dsn(user, token, host string, port int, database, sslmode string) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, token),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/" + database,
	}
	q := u.Query()
	q.Set("sslmode", sslmode)
	q.Set("connect_timeout", "10")
	u.RawQuery = q.Encode()
	return u.String()
}

func statementCount(suite conformance.Suite) int {
	n := len(suite.Setup) + len(suite.Cleanup)
	for _, c := range suite.Cases {
		n += len(c.Steps)
	}
	return n
}

func printSuite(suite conformance.Suite) {
	fmt.Printf("suite %s: %d cases, %d setup, %d cleanup\n", suite.Name, len(suite.Cases), len(suite.Setup), len(suite.Cleanup))
	for _, c := range suite.Cases {
		note := ""
		if c.Note != "" {
			note = "  (" + c.Note + ")"
		}
		gap := ""
		if c.KnownGap != "" {
			gap = "  [known gap: " + c.KnownGap + "]"
		}
		fmt.Printf("  %-28s %-12s %d step(s)%s%s\n", c.Name, c.Group, len(c.Steps), note, gap)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
