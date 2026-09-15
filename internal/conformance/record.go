package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Observation is what a target database did with one statement.
type Observation struct {
	Outcome    string     `json:"outcome"` // "ok" or "error"
	SQLState   string     `json:"sqlstate,omitempty"`
	Message    string     `json:"message,omitempty"`
	CommandTag string     `json:"command_tag,omitempty"`
	Columns    []string   `json:"columns,omitempty"`
	Rows       [][]string `json:"rows,omitempty"`
}

// RecordedCase pairs a probe with what the target did for each of its steps.
type RecordedCase struct {
	Case
	Observations []Observation `json:"observations"`
}

// Golden is a recorded baseline.
type Golden struct {
	RecordedAt    time.Time      `json:"recorded_at"`
	Target        string         `json:"target"`
	ServerVersion string         `json:"server_version,omitempty"`
	Suite         string         `json:"suite"`
	Cases         []RecordedCase `json:"cases"`
}

// ProgressFunc reports progress; it may be nil.
type ProgressFunc func(format string, args ...any)

// Observe runs one statement and captures the outcome. It uses the extended
// protocol, so multi-statement strings are not executed.
func Observe(ctx context.Context, conn *pgx.Conn, sql string) Observation {
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return failedObservation(err)
	}
	defer rows.Close()

	obs := Observation{Outcome: "ok"}
	for _, fd := range rows.FieldDescriptions() {
		obs.Columns = append(obs.Columns, fd.Name)
	}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return failedObservation(err)
		}
		obs.Rows = append(obs.Rows, formatValues(values))
	}
	if err := rows.Err(); err != nil {
		return failedObservation(err)
	}
	obs.CommandTag = rows.CommandTag().String()
	return obs
}

// RunSuite applies the schema, probes every case, and cleans up. Cleanup runs
// even when the run fails, so a cluster is left as it was found.
func RunSuite(ctx context.Context, conn *pgx.Conn, suite Suite, progress ProgressFunc) (*Golden, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}

	golden := &Golden{RecordedAt: time.Now().UTC(), Suite: suite.Name}
	if err := conn.QueryRow(ctx, "SELECT version()").Scan(&golden.ServerVersion); err != nil {
		progress("could not read server version: %v", err)
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		progress("cleanup: dropping %d objects", len(suite.Cleanup))
		for _, sql := range suite.Cleanup {
			if _, err := conn.Exec(cleanupCtx, sql); err != nil {
				progress("cleanup skipped %q: %v", sql, err)
			}
		}
	}()

	for _, sql := range suite.Setup {
		if _, err := conn.Exec(ctx, sql); err != nil {
			return golden, fmt.Errorf("setup %q: %w", sql, err)
		}
	}

	for _, c := range suite.Cases {
		recorded := RecordedCase{Case: c}
		for _, sql := range c.Steps {
			recorded.Observations = append(recorded.Observations, Observe(ctx, conn, sql))
		}

		// Any case can leave a transaction open, or aborted. Reset so the next
		// case starts clean; a ROLLBACK with nothing to undo is harmless.
		if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
			progress("reset rollback after %s failed: %v", c.Name, err)
		}

		golden.Cases = append(golden.Cases, recorded)
		progress("recorded %-28s %s", c.Name, summarize(recorded.Observations))
	}

	return golden, nil
}

func failedObservation(err error) Observation {
	obs := Observation{Outcome: "error", Message: err.Error()}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		obs.SQLState = pgErr.Code
		obs.Message = pgErr.Message
	}
	return obs
}

func formatValues(values []any) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = formatValue(v)
	}
	return out
}

func formatValue(v any) string {
	switch typed := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func summarize(observations []Observation) string {
	summary := ""
	for i, obs := range observations {
		if i > 0 {
			summary += " | "
		}
		if obs.Outcome == "error" {
			summary += "error " + obs.SQLState
		} else {
			summary += obs.CommandTag
		}
	}
	return summary
}

// Save writes a golden record to path, creating parent directories.
func Save(path string, golden *Golden) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// SaveDir writes one fixture per case group into dir, replacing any fixtures
// already there so that stale cases cannot linger. Grouping keeps each file
// small as the suite grows; add finer groups to split further.
func SaveDir(dir string, golden *Golden) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stale, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return err
		}
	}

	var order []string
	byGroup := make(map[string][]RecordedCase)
	for _, c := range golden.Cases {
		if c.Group == "" {
			return fmt.Errorf("case %q has no group to save it under", c.Name)
		}
		if _, seen := byGroup[c.Group]; !seen {
			order = append(order, c.Group)
		}
		byGroup[c.Group] = append(byGroup[c.Group], c)
	}

	for _, group := range order {
		part := *golden
		part.Cases = byGroup[group]
		if err := Save(filepath.Join(dir, group+".json"), &part); err != nil {
			return err
		}
	}
	return nil
}

// Load reads a golden record.
func Load(path string) (*Golden, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var golden Golden
	if err := json.Unmarshal(data, &golden); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &golden, nil
}

// LoadDir reads every fixture in dir and merges them into one record. Case
// names must be unique across fixtures.
func LoadDir(dir string) (*Golden, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no golden fixtures in %s", dir)
	}
	sort.Strings(paths)

	merged := &Golden{}
	origin := make(map[string]string)
	for _, path := range paths {
		part, err := Load(path)
		if err != nil {
			return nil, err
		}
		if merged.Suite == "" {
			merged.RecordedAt = part.RecordedAt
			merged.Target = part.Target
			merged.ServerVersion = part.ServerVersion
			merged.Suite = part.Suite
		} else if part.Suite != merged.Suite {
			return nil, fmt.Errorf("%s is suite %q, expected %q", path, part.Suite, merged.Suite)
		}
		for _, c := range part.Cases {
			if first, dup := origin[c.Name]; dup {
				return nil, fmt.Errorf("case %q appears in both %s and %s", c.Name, first, path)
			}
			origin[c.Name] = path
			merged.Cases = append(merged.Cases, c)
		}
	}
	sort.Slice(merged.Cases, func(i, j int) bool { return merged.Cases[i].Name < merged.Cases[j].Name })
	return merged, nil
}
