package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// Results holds one entry per statement when the step sent several, which
	// only a simple query can do. The fields above then carry the error, if the
	// query raised one.
	Results []Result `json:"results,omitempty"`
}

// Result is what one statement of a multi-statement simple query answered.
type Result struct {
	CommandTag string     `json:"command_tag,omitempty"`
	Columns    []string   `json:"columns,omitempty"`
	Rows       [][]string `json:"rows,omitempty"`
}

// RecordedCase pairs a probe with what the target did for each of its steps.
// A case uses either Observations (one connection) or Sessions (one list per
// concurrent connection).
type RecordedCase struct {
	Case
	Observations []Observation `json:"observations,omitempty"`
	// SessionResults holds one observation list per concurrent session.
	SessionResults [][]Observation `json:"sessions,omitempty"`
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

// Connector opens one connection to the target. Concurrency cases need more
// than one.
type Connector func(ctx context.Context) (*pgx.Conn, error)

// Options tune a run.
type Options struct {
	// IncludeRecordOnly runs cases marked RecordOnly. The recorder does; a
	// replay against the emulator does not, because those cases cannot be
	// reproduced there.
	IncludeRecordOnly bool
	// Progress reports progress; nil is fine.
	Progress ProgressFunc
}

const (
	// sessionStepDelay separates consecutive steps of a concurrent session so
	// the interleaving is deterministic without needing barriers.
	sessionStepDelay = 150 * time.Millisecond
	// sessionStepTimeout bounds a single step, so a blocking target cannot hang
	// the run.
	sessionStepTimeout = 15 * time.Second
)

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

// ObserveSimple runs one step as a simple query, which is the only way to send
// several statements at once -- what `psql -c 'a; b'` does. Every statement's
// result is captured, because the interesting part is often the second one.
func ObserveSimple(ctx context.Context, conn *pgx.Conn, sql string) Observation {
	results, err := conn.PgConn().Exec(ctx, sql).ReadAll()
	if err != nil {
		return failedObservation(err)
	}

	obs := Observation{Outcome: "ok"}
	for _, res := range results {
		one := Result{CommandTag: res.CommandTag.String()}
		for _, fd := range res.FieldDescriptions {
			one.Columns = append(one.Columns, string(fd.Name))
		}
		for _, row := range res.Rows {
			one.Rows = append(one.Rows, formatRawValues(row))
		}
		obs.Results = append(obs.Results, one)
	}
	return obs
}

// formatRawValues renders a row the way a value read through pgx is rendered,
// so a multi-statement result reads like any other.
func formatRawValues(row [][]byte) []string {
	out := make([]string, len(row))
	for i, v := range row {
		if v == nil {
			out[i] = "NULL"
			continue
		}
		out[i] = string(v)
	}
	return out
}

// RunSuite applies the schema, probes every case, and cleans up. Cleanup runs
// even when the run fails, so a cluster is left as it was found.
func RunSuite(ctx context.Context, connect Connector, suite Suite, opts Options) (*Golden, error) {
	progress := opts.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}

	conn, err := connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())

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

		switch {
		case c.RecordOnly && !opts.IncludeRecordOnly:
			progress("skipped  %-28s record-only", c.Name)
			continue
		case len(c.Sessions) > 0:
			sessions, err := runSessions(ctx, connect, c, progress)
			if err != nil {
				return golden, fmt.Errorf("case %s: %w", c.Name, err)
			}
			recorded.SessionResults = sessions
			progress("recorded %-28s %s", c.Name, summarizeSessions(sessions))
		default:
			observe := Observe
			if c.SimpleProtocol {
				observe = ObserveSimple
			}
			for _, sql := range c.Steps {
				recorded.Observations = append(recorded.Observations, observe(ctx, conn, sql))
			}
			// Any case can leave a transaction open, or aborted. Reset so the
			// next case starts clean; a ROLLBACK with nothing to undo is
			// harmless.
			if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
				progress("reset rollback after %s failed: %v", c.Name, err)
			}
			progress("recorded %-28s %s", c.Name, summarize(recorded.Observations))
		}

		golden.Cases = append(golden.Cases, recorded)
	}

	return golden, nil
}

// runSessions runs each session's steps on its own connection, concurrently and
// with a fixed delay between steps so the interleaving is deterministic.
func runSessions(ctx context.Context, connect Connector, c Case, progress ProgressFunc) ([][]Observation, error) {
	conns := make([]*pgx.Conn, len(c.Sessions))
	for i := range c.Sessions {
		conn, err := connect(ctx)
		if err != nil {
			return nil, fmt.Errorf("session %d connect: %w", i, err)
		}
		conns[i] = conn
		defer conn.Close(context.Background())
	}

	results := make([][]Observation, len(c.Sessions))
	var wg sync.WaitGroup
	for i, steps := range c.Sessions {
		wg.Add(1)
		go func(i int, steps []string) {
			defer wg.Done()
			for j, sql := range steps {
				if j > 0 {
					time.Sleep(sessionStepDelay)
				}
				stepCtx, cancel := context.WithTimeout(ctx, sessionStepTimeout)
				obs := Observe(stepCtx, conns[i], sql)
				timedOut := errors.Is(stepCtx.Err(), context.DeadlineExceeded)
				cancel()
				if timedOut {
					progress("session %d step %d timed out", i, j)
				}
				results[i] = append(results[i], obs)
			}
		}(i, steps)
	}
	wg.Wait()
	return results, nil
}

func summarizeSessions(sessions [][]Observation) string {
	out := ""
	for i, obs := range sessions {
		if i > 0 {
			out += " || "
		}
		out += "session " + itoa(i) + ": " + summarize(obs)
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

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
		switch {
		case obs.Outcome == "error":
			summary += "error " + obs.SQLState
		case len(obs.Results) > 0:
			// A step that sent several statements answered once per statement.
			for n, res := range obs.Results {
				if n > 0 {
					summary += " + "
				}
				summary += res.CommandTag
			}
		default:
			summary += obs.CommandTag
		}
	}
	return summary
}

// Save writes a golden record to path, creating parent directories, and
// reports whether it changed what was already there.
//
// A record whose content matches the one on disk is not written at all, and
// keeps the date it was recorded on. Re-recording costs a run against a real
// cluster and is done to find out whether the cluster still answers the same
// way; when it does, the answer is that nothing changed, and a fixture whose
// only difference is a new timestamp hides that in every diff it appears in.
// That a run happened at all belongs in the progress log, not in the fixtures.
func Save(path string, golden *Golden) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}

	if sameContent(path, golden) {
		return false, nil
	}

	data, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(data, '\n'), 0o644)
}

// sameContent reports whether the record on disk at path observed the same
// thing as golden. Both are rendered through the same marshaller, so every
// field counts, including ones added to the record later, without an equality
// function to keep in step with the struct.
func sameContent(path string, golden *Golden) bool {
	existing, err := Load(path)
	if err != nil {
		return false
	}
	was, err := contentBytes(existing)
	if err != nil {
		return false
	}
	now, err := contentBytes(golden)
	if err != nil {
		return false
	}
	return bytes.Equal(was, now)
}

// contentBytes renders what a record holds the emulator to, rather than
// everything it happened to see. A run observes things that differ every time
// and are enforced against nothing: a generated job id, and which of two
// transactions lost a race. Comparing those would put a diff in the record's
// history for every run and bury the ones that found a real change.
func contentBytes(golden *Golden) ([]byte, error) {
	content := *golden
	content.RecordedAt = time.Time{}
	content.Cases = make([]RecordedCase, len(golden.Cases))
	for i, c := range golden.Cases {
		content.Cases[i] = enforcedCase(c)
	}
	return json.Marshal(&content)
}

// enforcedCase drops from a copy of a case whatever the comparison declines to
// enforce, so change detection is exactly as sensitive as the replay it guards.
func enforcedCase(c RecordedCase) RecordedCase {
	if c.IgnoreRows {
		c.Observations = withoutRows(c.Observations)
		sessions := make([][]Observation, len(c.SessionResults))
		for i, s := range c.SessionResults {
			sessions[i] = withoutRows(s)
		}
		c.SessionResults = sessions
	}
	if c.ConflictRace {
		c.SessionResults = withConflictFinalsPooled(c.SessionResults)
	}
	return c
}

// withoutRows clears what IgnoreRows waives: the rows themselves, and the
// command tag when all it carries is how many there were.
func withoutRows(obs []Observation) []Observation {
	out := make([]Observation, len(obs))
	for i, o := range obs {
		o.Rows = nil
		if isRowCountTag(o.CommandTag) {
			o.CommandTag = ""
		}
		out[i] = o
	}
	return out
}

// withConflictFinalsPooled moves the step a conflict surfaces at out of its
// session and into a sorted pool, so a ConflictRace case is compared on how
// many transactions lost rather than on which of them did. Every earlier step
// stays where it was, because those are enforced session by session.
func withConflictFinalsPooled(sessions [][]Observation) [][]Observation {
	pooled := make([][]Observation, 0, len(sessions)+1)
	var finals []Observation
	for _, s := range sessions {
		if len(s) == 0 {
			pooled = append(pooled, s)
			continue
		}
		pooled = append(pooled, s[:len(s)-1])
		finals = append(finals, s[len(s)-1])
	}
	sort.Slice(finals, func(i, j int) bool { return finalKey(finals[i]) < finalKey(finals[j]) })
	return append(pooled, finals)
}

func finalKey(o Observation) string {
	return strings.Join([]string{o.Outcome, o.SQLState, o.CommandTag, o.Message}, "\x00")
}

// SaveDir writes one fixture per case group into dir, replacing any fixtures
// already there so that stale cases cannot linger. Grouping keeps each file
// small as the suite grows; add finer groups to split further.
//
// It returns the groups whose content changed; a group that answered exactly as
// it did before is left on disk untouched, timestamp included.
//
// A record with no cases is refused, and stale fixtures are pruned only once
// every new one is on disk: a golden record costs a run against a real cluster,
// so a failed save must never be able to leave the directory empty.
func SaveDir(dir string, golden *Golden) ([]string, error) {
	if golden == nil || len(golden.Cases) == 0 {
		return nil, errors.New("conformance: refusing to save a golden record with no cases")
	}

	var order []string
	byGroup := make(map[string][]RecordedCase)
	for _, c := range golden.Cases {
		if c.Group == "" {
			return nil, fmt.Errorf("case %q has no group to save it under", c.Name)
		}
		if _, seen := byGroup[c.Group]; !seen {
			order = append(order, c.Group)
		}
		byGroup[c.Group] = append(byGroup[c.Group], c)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	var changed []string
	written := make(map[string]bool, len(order))
	for _, group := range order {
		part := *golden
		part.Cases = byGroup[group]
		path := filepath.Join(dir, group+".json")
		wrote, err := Save(path, &part)
		if err != nil {
			return nil, err
		}
		if wrote {
			changed = append(changed, group)
		}
		written[path] = true
	}

	stale, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range stale {
		if written[path] {
			continue
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		changed = append(changed, strings.TrimSuffix(filepath.Base(path), ".json"))
	}
	sort.Strings(changed)
	return changed, nil
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
