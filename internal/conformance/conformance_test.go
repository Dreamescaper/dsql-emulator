package conformance_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dreamescaper/dsql-emulator/internal/conformance"
)

func TestDefaultSuiteIsWellFormed(t *testing.T) {
	suite := conformance.DefaultSuite()

	if suite.Name == "" {
		t.Fatal("suite has no name")
	}
	if len(suite.Setup) == 0 || len(suite.Cleanup) == 0 {
		t.Fatal("suite must set up and clean up schema")
	}
	if len(suite.Cases) == 0 {
		t.Fatal("suite has no cases")
	}

	seen := make(map[string]bool, len(suite.Cases))
	for _, c := range suite.Cases {
		if c.Name == "" {
			t.Fatal("case with no name")
		}
		if seen[c.Name] {
			t.Fatalf("duplicate case %q", c.Name)
		}
		seen[c.Name] = true

		if c.Group == "" {
			t.Fatalf("case %q has no group", c.Name)
		}
		if len(c.Steps) == 0 && len(c.Sessions) == 0 {
			t.Fatalf("case %q has no steps", c.Name)
		}
		if len(c.Steps) > 0 && len(c.Sessions) > 0 {
			t.Fatalf("case %q mixes single-session and concurrent steps", c.Name)
		}
		for _, step := range c.Steps {
			if step == "" {
				t.Fatalf("case %q has an empty step", c.Name)
			}
		}
		for i, session := range c.Sessions {
			if len(session) == 0 {
				t.Fatalf("case %q session %d has no steps", c.Name, i)
			}
		}
	}
}

func TestGoldenRoundTrip(t *testing.T) {
	golden := &conformance.Golden{
		RecordedAt:    time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		Target:        "aurora-dsql",
		ServerVersion: "PostgreSQL 16.x",
		Suite:         "dsql-baseline",
		Cases: []conformance.RecordedCase{{
			Case: conformance.Case{Name: "select_one", Group: "supported", Steps: []string{"SELECT 1"}},
			Observations: []conformance.Observation{{
				Outcome:    "ok",
				CommandTag: "SELECT 1",
				Columns:    []string{"?column?"},
				Rows:       [][]string{{"1"}},
			}},
		}},
	}

	path := filepath.Join(t.TempDir(), "golden", "dsql.json")
	if _, err := conformance.Save(path, golden); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := conformance.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Suite != golden.Suite || loaded.Target != golden.Target {
		t.Fatalf("round trip changed metadata: %+v", loaded)
	}
	if len(loaded.Cases) != 1 || loaded.Cases[0].Name != "select_one" {
		t.Fatalf("round trip changed cases: %+v", loaded.Cases)
	}
	if got := loaded.Cases[0].Observations[0].Rows[0][0]; got != "1" {
		t.Fatalf("round trip changed rows: %q", got)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := conformance.Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected an error for a missing golden file")
	}
}

func caseInGroup(name, group string) conformance.RecordedCase {
	return conformance.RecordedCase{
		Case:         conformance.Case{Name: name, Group: group, Steps: []string{"SELECT 1"}},
		Observations: []conformance.Observation{{Outcome: "ok", CommandTag: "SELECT 1"}},
	}
}

func TestSaveDirAndLoadDirRoundTrip(t *testing.T) {
	dir := t.TempDir()
	golden := &conformance.Golden{
		Target:        "aurora-dsql",
		ServerVersion: "PostgreSQL 16",
		Suite:         "dsql-baseline",
		Cases: []conformance.RecordedCase{
			caseInGroup("a", "supported"),
			caseInGroup("b", "supported"),
			caseInGroup("c", "unsupported"),
		},
	}

	if _, err := conformance.SaveDir(dir, golden); err != nil {
		t.Fatalf("save dir: %v", err)
	}
	for _, name := range []string{"supported.json", "unsupported.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected fixture %s: %v", name, err)
		}
	}

	loaded, err := conformance.LoadDir(dir)
	if err != nil {
		t.Fatalf("load dir: %v", err)
	}
	if len(loaded.Cases) != 3 {
		t.Fatalf("got %d cases want 3", len(loaded.Cases))
	}
	if loaded.Target != "aurora-dsql" || loaded.ServerVersion != "PostgreSQL 16" || loaded.Suite != "dsql-baseline" {
		t.Fatalf("metadata lost: %+v", loaded)
	}
	for _, c := range loaded.Cases {
		if len(c.Observations) != 1 {
			t.Fatalf("case %q lost its observations", c.Name)
		}
	}
}

// Recording a golden record costs a run against a real cluster, so a save that
// would leave the directory empty is refused rather than performed.
func TestSaveDirRefusesToEmptyTheRecord(t *testing.T) {
	dir := t.TempDir()
	existing := &conformance.Golden{Suite: "s", Cases: []conformance.RecordedCase{caseInGroup("a", "supported")}}
	if _, err := conformance.SaveDir(dir, existing); err != nil {
		t.Fatalf("save dir: %v", err)
	}

	for _, empty := range []*conformance.Golden{nil, {Suite: "s"}} {
		if _, err := conformance.SaveDir(dir, empty); err == nil {
			t.Fatal("expected a record with no cases to be refused")
		}
	}

	loaded, err := conformance.LoadDir(dir)
	if err != nil {
		t.Fatalf("load dir: %v", err)
	}
	if len(loaded.Cases) != 1 {
		t.Fatalf("the existing record was disturbed: %d cases, want 1", len(loaded.Cases))
	}
}

func TestSaveDirReplacesStaleFixtures(t *testing.T) {
	dir := t.TempDir()
	first := &conformance.Golden{Suite: "s", Cases: []conformance.RecordedCase{caseInGroup("a", "old_group")}}
	if _, err := conformance.SaveDir(dir, first); err != nil {
		t.Fatalf("save dir: %v", err)
	}

	second := &conformance.Golden{Suite: "s", Cases: []conformance.RecordedCase{caseInGroup("a", "new_group")}}
	if _, err := conformance.SaveDir(dir, second); err != nil {
		t.Fatalf("save dir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "old_group.json")); !os.IsNotExist(err) {
		t.Fatalf("stale fixture was not removed (err=%v)", err)
	}
	loaded, err := conformance.LoadDir(dir)
	if err != nil {
		t.Fatalf("load dir: %v", err)
	}
	if len(loaded.Cases) != 1 || loaded.Cases[0].Name != "a" {
		t.Fatalf("unexpected cases: %+v", loaded.Cases)
	}
}

func TestLoadDirRejectsDuplicateCaseNames(t *testing.T) {
	dir := t.TempDir()
	for _, group := range []string{"one", "two"} {
		g := &conformance.Golden{Suite: "s", Cases: []conformance.RecordedCase{caseInGroup("dup", group)}}
		if _, err := conformance.Save(filepath.Join(dir, group+".json"), g); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if _, err := conformance.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a duplicated case name")
	}
}

func TestCompareConcurrentSessions(t *testing.T) {
	golden := conformance.RecordedCase{
		Case: conformance.Case{Name: "c", Sessions: [][]string{{"BEGIN"}, {"BEGIN", "COMMIT"}}},
		SessionResults: [][]conformance.Observation{
			{{Outcome: "ok", CommandTag: "BEGIN"}},
			{{Outcome: "ok", CommandTag: "BEGIN"}, {Outcome: "error", SQLState: "40001"}},
		},
	}
	emulated := golden

	if diffs := conformance.Failures(conformance.Compare(golden, emulated)); len(diffs) != 0 {
		t.Fatalf("expected no differences, got %v", diffs)
	}

	emulated.SessionResults = [][]conformance.Observation{
		{{Outcome: "ok", CommandTag: "BEGIN"}},
		{{Outcome: "ok", CommandTag: "BEGIN"}, {Outcome: "ok", CommandTag: "COMMIT"}},
	}
	diffs := conformance.Failures(conformance.Compare(golden, emulated))
	if len(diffs) == 0 || diffs[0].Session != 1 {
		t.Fatalf("expected a difference in session 1, got %v", diffs)
	}
}

func TestCompareSkipsRecordOnlyCases(t *testing.T) {
	golden := &conformance.Golden{Cases: []conformance.RecordedCase{
		{Case: conformance.Case{Name: "conflict", RecordOnly: true}, SessionResults: [][]conformance.Observation{{{Outcome: "error"}}}},
	}}
	emulated := &conformance.Golden{}

	if diffs := conformance.CompareSuites(golden, emulated); len(diffs) != 0 {
		t.Fatalf("record-only cases must not be compared, got %v", diffs)
	}
}

func TestCompareIgnoresRowCountTagWhenRowsAreIgnored(t *testing.T) {
	golden := conformance.RecordedCase{
		Case:         conformance.Case{Name: "c", Steps: []string{"SELECT * FROM sys.jobs"}, IgnoreRows: true},
		Observations: []conformance.Observation{{Outcome: "ok", CommandTag: "SELECT 7", Columns: []string{"job_id"}}},
	}
	emulated := conformance.RecordedCase{
		Case:         conformance.Case{Name: "c", Steps: []string{"SELECT * FROM sys.jobs"}, IgnoreRows: true},
		Observations: []conformance.Observation{{Outcome: "ok", CommandTag: "SELECT 1", Columns: []string{"job_id"}}},
	}

	if diffs := conformance.Failures(conformance.Compare(golden, emulated)); len(diffs) != 0 {
		t.Fatalf("expected the row-count tag to be ignored, got %v", diffs)
	}
}

func TestUnrecorded(t *testing.T) {
	golden := &conformance.Golden{Cases: []conformance.RecordedCase{caseInGroup("a", "g")}}
	emulated := &conformance.Golden{Cases: []conformance.RecordedCase{
		caseInGroup("a", "g"),
		caseInGroup("c", "g"),
		caseInGroup("b", "g"),
	}}

	got := conformance.Unrecorded(golden, emulated)
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("got %v want [b c]", got)
	}
}

func TestUnrecordedEmptyWhenFullyCovered(t *testing.T) {
	golden := &conformance.Golden{Cases: []conformance.RecordedCase{caseInGroup("a", "g")}}
	emulated := &conformance.Golden{Cases: []conformance.RecordedCase{caseInGroup("a", "g")}}

	if got := conformance.Unrecorded(golden, emulated); len(got) != 0 {
		t.Fatalf("got %v want none", got)
	}
}

func TestLoadDirMissingDirectory(t *testing.T) {
	if _, err := conformance.LoadDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected an error for a directory with no fixtures")
	}
}

func recorded(name string, steps []string, observations ...conformance.Observation) conformance.RecordedCase {
	return conformance.RecordedCase{
		Case:         conformance.Case{Name: name, Steps: steps},
		Observations: observations,
	}
}

func ok(tag string) conformance.Observation {
	return conformance.Observation{Outcome: "ok", CommandTag: tag}
}

func failure(sqlstate, message string) conformance.Observation {
	return conformance.Observation{Outcome: "error", SQLState: sqlstate, Message: message}
}

func TestCompareIdentical(t *testing.T) {
	golden := recorded("c", []string{"SELECT 1"}, ok("SELECT 1"))
	if diffs := conformance.Compare(golden, golden); len(diffs) != 0 {
		t.Fatalf("expected no differences, got %v", diffs)
	}
}

func TestCompareOutcomeDifference(t *testing.T) {
	golden := recorded("c", []string{"TRUNCATE t"}, failure("0A000", "not supported"))
	emulated := recorded("c", []string{"TRUNCATE t"}, ok("TRUNCATE TABLE"))

	diffs := conformance.Failures(conformance.Compare(golden, emulated))
	if len(diffs) == 0 {
		t.Fatal("expected an outcome difference")
	}
	if diffs[0].Field != "outcome" {
		t.Fatalf("got field %q want outcome", diffs[0].Field)
	}
}

func TestCompareSQLStateDifference(t *testing.T) {
	golden := recorded("c", []string{"X"}, failure("0A000", "no"))
	emulated := recorded("c", []string{"X"}, failure("42601", "no"))

	diffs := conformance.Failures(conformance.Compare(golden, emulated))
	if len(diffs) != 1 || diffs[0].Field != "sqlstate" {
		t.Fatalf("expected one sqlstate difference, got %v", diffs)
	}
}

func TestCompareMessageIsAdvisory(t *testing.T) {
	golden := recorded("c", []string{"X"}, failure("0A000", "TRUNCATE is not supported"))
	emulated := recorded("c", []string{"X"}, failure("0A000", "truncate not supported"))

	all := conformance.Compare(golden, emulated)
	if len(all) != 1 || all[0].Field != "message" || !all[0].Advisory {
		t.Fatalf("expected one advisory message difference, got %v", all)
	}
	if failures := conformance.Failures(all); len(failures) != 0 {
		t.Fatalf("message differences must not fail, got %v", failures)
	}
}

func TestCompareResultDifferences(t *testing.T) {
	golden := conformance.RecordedCase{
		Case: conformance.Case{Name: "c", Steps: []string{"SELECT 1"}},
		Observations: []conformance.Observation{{
			Outcome: "ok", CommandTag: "SELECT 1",
			Columns: []string{"?column?"}, Rows: [][]string{{"1"}},
		}},
	}
	emulated := conformance.RecordedCase{
		Case: conformance.Case{Name: "c", Steps: []string{"SELECT 1"}},
		Observations: []conformance.Observation{{
			Outcome: "ok", CommandTag: "SELECT 2",
			Columns: []string{"?column?"}, Rows: [][]string{{"2"}},
		}},
	}

	diffs := conformance.Failures(conformance.Compare(golden, emulated))
	var fields []string
	for _, d := range diffs {
		fields = append(fields, d.Field)
	}
	if len(fields) != 2 || fields[0] != "command_tag" || fields[1] != "rows" {
		t.Fatalf("got fields %v want [command_tag rows]", fields)
	}
}

func TestCompareIgnoreRows(t *testing.T) {
	golden := conformance.RecordedCase{
		Case:         conformance.Case{Name: "c", Steps: []string{"SELECT version()"}, IgnoreRows: true},
		Observations: []conformance.Observation{ok("SELECT 1")},
	}
	emulated := conformance.RecordedCase{
		Case:         conformance.Case{Name: "c", Steps: []string{"SELECT version()"}, IgnoreRows: true},
		Observations: []conformance.Observation{{Outcome: "ok", CommandTag: "SELECT 1", Rows: [][]string{{"different"}}}},
	}

	if diffs := conformance.Failures(conformance.Compare(golden, emulated)); len(diffs) != 0 {
		t.Fatalf("expected rows to be ignored, got %v", diffs)
	}
}

func TestCompareStepCountDifference(t *testing.T) {
	golden := recorded("c", []string{"BEGIN", "COMMIT"}, ok("BEGIN"), ok("COMMIT"))
	emulated := recorded("c", []string{"BEGIN", "COMMIT"}, ok("BEGIN"))

	diffs := conformance.Failures(conformance.Compare(golden, emulated))
	if len(diffs) != 1 || diffs[0].Field != "step_count" {
		t.Fatalf("expected a step_count difference, got %v", diffs)
	}
}

// A re-recording that found the same answers must leave the fixture exactly as
// it was, so a diff of the golden record shows the runs that changed something
// rather than every run that happened.
func TestSaveLeavesAnUnchangedRecordAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")

	first := &conformance.Golden{
		RecordedAt: time.Date(2026, 9, 16, 18, 30, 0, 0, time.UTC),
		Target:     "aurora-dsql",
		Suite:      "s",
		Cases:      []conformance.RecordedCase{caseInGroup("a", "g")},
	}
	if changed, err := conformance.Save(path, first); err != nil || !changed {
		t.Fatalf("first save: changed=%v err=%v, want changed", changed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// The same observations, recorded a day later.
	again := *first
	again.RecordedAt = time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	changed, err := conformance.Save(path, &again)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if changed {
		t.Error("a record that observed the same thing was reported as changed")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the fixture was rewritten:\nbefore %s\nafter  %s", before, after)
	}
}

// An observation that differs is what a re-recording exists to catch, so the
// fixture is rewritten, timestamp and all.
func TestSaveRewritesAChangedRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")

	recorded := time.Date(2026, 9, 16, 18, 30, 0, 0, time.UTC)
	first := &conformance.Golden{
		RecordedAt: recorded,
		Suite:      "s",
		Cases:      []conformance.RecordedCase{caseInGroup("a", "g")},
	}
	if _, err := conformance.Save(path, first); err != nil {
		t.Fatalf("first save: %v", err)
	}

	second := &conformance.Golden{
		RecordedAt: recorded.Add(24 * time.Hour),
		Suite:      "s",
		Cases:      []conformance.RecordedCase{caseInGroup("a", "g")},
	}
	second.Cases[0].Observations = []conformance.Observation{{Outcome: "error", SQLState: "0A000"}}

	changed, err := conformance.Save(path, second)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if !changed {
		t.Fatal("a record with a different observation was reported as unchanged")
	}

	loaded, err := conformance.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.RecordedAt.Equal(second.RecordedAt) {
		t.Errorf("got recorded_at %s want %s", loaded.RecordedAt, second.RecordedAt)
	}
}

// SaveDir reports which groups changed, so a run that found something can be
// told apart from one that did not without reading the diff.
func TestSaveDirReportsOnlyTheGroupsThatChanged(t *testing.T) {
	dir := t.TempDir()
	golden := &conformance.Golden{
		RecordedAt: time.Date(2026, 9, 16, 18, 30, 0, 0, time.UTC),
		Suite:      "s",
		Cases: []conformance.RecordedCase{
			caseInGroup("a", "one"),
			caseInGroup("b", "two"),
		},
	}
	changed, err := conformance.SaveDir(dir, golden)
	if err != nil {
		t.Fatalf("save dir: %v", err)
	}
	if len(changed) != 2 {
		t.Fatalf("the first save reported %v, want both groups", changed)
	}

	// Recorded again a day later, with one group answering differently.
	again := *golden
	again.RecordedAt = again.RecordedAt.Add(24 * time.Hour)
	again.Cases = []conformance.RecordedCase{caseInGroup("a", "one"), caseInGroup("b", "two")}
	again.Cases[1].Observations = []conformance.Observation{{Outcome: "error", SQLState: "42601"}}

	changed, err = conformance.SaveDir(dir, &again)
	if err != nil {
		t.Fatalf("save dir: %v", err)
	}
	if len(changed) != 1 || changed[0] != "two" {
		t.Fatalf("got %v, want only the group that changed", changed)
	}

	one, err := conformance.Load(filepath.Join(dir, "one.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !one.RecordedAt.Equal(golden.RecordedAt) {
		t.Errorf("the unchanged group was re-dated to %s", one.RecordedAt)
	}
}

// A fixture removed because its group is gone counts as a change: the record
// is not what it was, even though nothing was written.
func TestSaveDirReportsPrunedGroups(t *testing.T) {
	dir := t.TempDir()
	first := &conformance.Golden{Suite: "s", Cases: []conformance.RecordedCase{caseInGroup("a", "old_group")}}
	if _, err := conformance.SaveDir(dir, first); err != nil {
		t.Fatalf("save dir: %v", err)
	}

	second := &conformance.Golden{Suite: "s", Cases: []conformance.RecordedCase{caseInGroup("b", "new_group")}}
	changed, err := conformance.SaveDir(dir, second)
	if err != nil {
		t.Fatalf("save dir: %v", err)
	}
	if len(changed) != 2 || changed[0] != "new_group" || changed[1] != "old_group" {
		t.Fatalf("got %v, want both the new and the pruned group", changed)
	}
}

// A generated id differs on every run and is enforced against nothing, so the
// case that records it declares IgnoreRows and its fixture must not be
// rewritten for it.
func TestSaveIgnoresRowsTheRecordDoesNotEnforce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")

	withJobID := func(id string) *conformance.Golden {
		return &conformance.Golden{
			Suite: "s",
			Cases: []conformance.RecordedCase{{
				Case: conformance.Case{Name: "a", Group: "g", Steps: []string{"CREATE INDEX ASYNC i ON t (a)"}, IgnoreRows: true},
				Observations: []conformance.Observation{{
					Outcome:    "ok",
					CommandTag: "SELECT 1",
					Columns:    []string{"job_id"},
					Rows:       [][]string{{id}},
				}},
			}},
		}
	}

	if changed, err := conformance.Save(path, withJobID("6putf6tsgndd5li4ks7biloseu")); err != nil || !changed {
		t.Fatalf("first save: changed=%v err=%v", changed, err)
	}
	changed, err := conformance.Save(path, withJobID("jhbdh3rmgjfv5fh7vcwocotelq"))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if changed {
		t.Error("a fixture was rewritten for a generated id the record ignores")
	}
}

// A column that appears or disappears is enforced even when the rows are not,
// so it must still rewrite the fixture.
func TestSaveStillNoticesColumnsWhenRowsAreIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")

	withColumns := func(cols ...string) *conformance.Golden {
		return &conformance.Golden{
			Suite: "s",
			Cases: []conformance.RecordedCase{{
				Case:         conformance.Case{Name: "a", Group: "g", IgnoreRows: true},
				Observations: []conformance.Observation{{Outcome: "ok", Columns: cols, Rows: [][]string{{"x"}}}},
			}},
		}
	}

	if _, err := conformance.Save(path, withColumns("job_id")); err != nil {
		t.Fatalf("first save: %v", err)
	}
	changed, err := conformance.Save(path, withColumns("job_id", "status"))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if !changed {
		t.Error("a new column went unnoticed because the rows are ignored")
	}
}

// Which transaction loses a conflict is a race, so a run that swapped the
// winner and loser recorded the same thing and must not rewrite the fixture.
func TestSaveIgnoresWhichSessionLostARace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")

	ok := conformance.Observation{Outcome: "ok", CommandTag: "COMMIT"}
	lost := conformance.Observation{Outcome: "error", SQLState: "40001", Message: "change conflicts with another transaction (OC000)"}
	wrote := conformance.Observation{Outcome: "ok", CommandTag: "UPDATE 1"}

	race := func(first, second conformance.Observation) *conformance.Golden {
		return &conformance.Golden{
			Suite: "s",
			Cases: []conformance.RecordedCase{{
				Case: conformance.Case{Name: "a", Group: "g", ConflictRace: true},
				SessionResults: [][]conformance.Observation{
					{wrote, first},
					{wrote, second},
				},
			}},
		}
	}

	if changed, err := conformance.Save(path, race(lost, ok)); err != nil || !changed {
		t.Fatalf("first save: changed=%v err=%v", changed, err)
	}
	changed, err := conformance.Save(path, race(ok, lost))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if changed {
		t.Error("a fixture was rewritten because the race went the other way")
	}

	// Both transactions committing is a different answer, and must be recorded.
	changed, err = conformance.Save(path, race(ok, ok))
	if err != nil {
		t.Fatalf("third save: %v", err)
	}
	if !changed {
		t.Error("a conflict that stopped happening went unnoticed")
	}
}

// Only the step the conflict surfaces at is a race; an earlier step is still
// enforced session by session.
func TestSaveNoticesAnEarlierStepInARaceCase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")

	lost := conformance.Observation{Outcome: "error", SQLState: "40001"}
	ok := conformance.Observation{Outcome: "ok", CommandTag: "COMMIT"}

	race := func(tag string) *conformance.Golden {
		return &conformance.Golden{
			Suite: "s",
			Cases: []conformance.RecordedCase{{
				Case: conformance.Case{Name: "a", Group: "g", ConflictRace: true},
				SessionResults: [][]conformance.Observation{
					{{Outcome: "ok", CommandTag: tag}, lost},
					{{Outcome: "ok", CommandTag: "UPDATE 1"}, ok},
				},
			}},
		}
	}

	if _, err := conformance.Save(path, race("UPDATE 1")); err != nil {
		t.Fatalf("first save: %v", err)
	}
	changed, err := conformance.Save(path, race("UPDATE 2"))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if !changed {
		t.Error("a changed row count before the commit went unnoticed")
	}
}

// multiCase builds a case whose single step sent several statements.
func multiCase(ignoreRows bool, results ...conformance.Result) conformance.RecordedCase {
	return conformance.RecordedCase{
		Case: conformance.Case{
			Name: "m", Group: "multi_statement", SimpleProtocol: true,
			Steps: []string{"BEGIN; SELECT 1; COMMIT"}, IgnoreRows: ignoreRows,
		},
		Observations: []conformance.Observation{{Outcome: "ok", Results: results}},
	}
}

// A step that sent several statements answered once per statement, and each
// answer is enforced: the interesting one is rarely the first.
func TestCompareChecksEveryStatementOfAMultiStatementStep(t *testing.T) {
	tests := []struct {
		name    string
		golden  conformance.RecordedCase
		emulate conformance.RecordedCase
		want    string
	}{
		{
			name:    "a statement that answered nothing",
			golden:  multiCase(false, conformance.Result{CommandTag: "BEGIN"}, conformance.Result{CommandTag: "SELECT 1"}),
			emulate: multiCase(false, conformance.Result{CommandTag: "BEGIN"}),
			want:    "result_count",
		},
		{
			name:    "a different tag on the second statement",
			golden:  multiCase(false, conformance.Result{CommandTag: "BEGIN"}, conformance.Result{CommandTag: "CREATE INDEX"}),
			emulate: multiCase(false, conformance.Result{CommandTag: "BEGIN"}, conformance.Result{CommandTag: "SELECT 1"}),
			want:    "result 1 command_tag",
		},
		{
			name:    "a column that appeared on the second statement",
			golden:  multiCase(false, conformance.Result{CommandTag: "BEGIN"}, conformance.Result{CommandTag: "CREATE INDEX"}),
			emulate: multiCase(false, conformance.Result{CommandTag: "BEGIN"}, conformance.Result{CommandTag: "CREATE INDEX", Columns: []string{"job_id"}}),
			want:    "result 1 columns",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diffs := conformance.Failures(conformance.Compare(tt.golden, tt.emulate))
			if len(diffs) == 0 {
				t.Fatalf("expected a difference in %s", tt.want)
			}
			if diffs[0].Field != tt.want {
				t.Fatalf("got field %q want %q", diffs[0].Field, tt.want)
			}
		})
	}
}

// A generated job id rides on one of those statements, so IgnoreRows has to
// reach inside them too.
func TestCompareIgnoresMultiStatementRowsWhenAsked(t *testing.T) {
	golden := multiCase(true,
		conformance.Result{CommandTag: "BEGIN"},
		conformance.Result{CommandTag: "CREATE INDEX", Columns: []string{"job_id"}, Rows: [][]string{{"srnwmlngyfhcndf5g2e6f3a3xm"}}})
	emulated := multiCase(true,
		conformance.Result{CommandTag: "BEGIN"},
		conformance.Result{CommandTag: "CREATE INDEX", Columns: []string{"job_id"}, Rows: [][]string{{"77014860-8193-95f5-d5ae-d8e80a35b166"}}})

	if diffs := conformance.Failures(conformance.Compare(golden, emulated)); len(diffs) != 0 {
		t.Fatalf("generated ids must not be enforced, got %v", diffs)
	}
}

// A generated id rides on one statement of a multi-statement step as readily as
// on a single-statement one, so IgnoreRows has to reach into Results for change
// detection too, not only for comparison.
func TestSaveIgnoresGeneratedRowsInsideAMultiStatementStep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")

	withJobID := func(id string) *conformance.Golden {
		return &conformance.Golden{
			Suite: "s",
			Cases: []conformance.RecordedCase{{
				Case: conformance.Case{
					Name: "a", Group: "g", SimpleProtocol: true, IgnoreRows: true,
					Steps: []string{"BEGIN; CREATE INDEX ASYNC i ON t (a); COMMIT"},
				},
				Observations: []conformance.Observation{{Outcome: "ok", Results: []conformance.Result{
					{CommandTag: "BEGIN"},
					{CommandTag: "CREATE INDEX", Columns: []string{"job_id"}, Rows: [][]string{{id}}},
					{CommandTag: "COMMIT"},
				}}},
			}},
		}
	}

	if changed, err := conformance.Save(path, withJobID("3im2i7eszjgxtgz6qku7dc5rv4")); err != nil || !changed {
		t.Fatalf("first save: changed=%v err=%v", changed, err)
	}
	changed, err := conformance.Save(path, withJobID("x6uomedydvez5a6zn43bgvn2g4"))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if changed {
		t.Error("a fixture was rewritten for a generated id inside a multi-statement step")
	}
}
