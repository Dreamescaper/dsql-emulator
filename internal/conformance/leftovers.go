package conformance

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Leftovers counts objects the suite may have created that are still present,
// so a run can prove it left the target as it found it. Targets that do not
// support a given catalog view simply skip that check.
func Leftovers(ctx context.Context, conn *pgx.Conn) map[string]int {
	checks := []struct {
		name  string
		query string
	}{
		{"tables", `SELECT count(*) FROM information_schema.tables WHERE table_name LIKE 'baseline\_%'`},
		{"sequences", `SELECT count(*) FROM information_schema.sequences WHERE sequence_name LIKE 'baseline\_%'`},
		{"routines", `SELECT count(*) FROM information_schema.routines WHERE routine_name LIKE 'baseline\_%'`},
		{"domains", `SELECT count(*) FROM information_schema.domains WHERE domain_name LIKE 'baseline\_%'`},
		{"schemas", `SELECT count(*) FROM information_schema.schemata WHERE schema_name LIKE 'baseline\_%'`},
		{"types", `SELECT count(*) FROM pg_type WHERE typname LIKE 'baseline\_%'`},
	}

	out := make(map[string]int)
	for _, check := range checks {
		var count int
		if err := conn.QueryRow(ctx, check.query).Scan(&count); err != nil {
			continue
		}
		if count > 0 {
			out[check.name] = count
		}
	}
	return out
}
