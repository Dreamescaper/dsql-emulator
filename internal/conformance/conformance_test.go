package conformance_test

import (
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
		if len(c.Steps) == 0 {
			t.Fatalf("case %q has no steps", c.Name)
		}
		for _, step := range c.Steps {
			if step == "" {
				t.Fatalf("case %q has an empty step", c.Name)
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
	if err := conformance.Save(path, golden); err != nil {
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

	if err := conformance.SaveDir(dir, golden); err != nil {
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

func TestSaveDirReplacesStaleFixtures(t *testing.T) {
	dir := t.TempDir()
	first := &conformance.Golden{Suite: "s", Cases: []conformance.RecordedCase{caseInGroup("a", "old_group")}}
	if err := conformance.SaveDir(dir, first); err != nil {
		t.Fatalf("save dir: %v", err)
	}

	second := &conformance.Golden{Suite: "s", Cases: []conformance.RecordedCase{caseInGroup("a", "new_group")}}
	if err := conformance.SaveDir(dir, second); err != nil {
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
		if err := conformance.Save(filepath.Join(dir, group+".json"), g); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if _, err := conformance.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a duplicated case name")
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
