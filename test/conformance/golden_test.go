package conformance_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dreamescaper/dsql-emulator/internal/conformance"
)

const goldenDir = "golden"

// TestRerecordingAnUnchangedClusterWritesNothing checks that a baseline run
// that found exactly what is already recorded rewrites no fixture. Recording
// costs a run against a real cluster, and the answer it usually brings back is
// that nothing changed; a save that rewrote the fixtures anyway would put a new
// timestamp in every diff and bury the runs that did find something.
//
// The recorder is simulated rather than run: the committed observations are put
// back into the order a run produces them in, which is the suite's, and saved.
// That needs no cluster and no Docker, so the property is checked on every
// `make test`. It also pins the record to the order its probes ran in, which is
// what makes a case that reads what an earlier one wrote readable.
func TestRerecordingAnUnchangedClusterWritesNothing(t *testing.T) {
	recorded, err := conformance.LoadDir(goldenDir)
	if err != nil {
		t.Fatalf("load the golden record: %v", err)
	}

	rerun := *recorded
	rerun.Cases = inSuiteOrder(t, recorded.Cases)
	// A run reports the time it happened; an unchanged fixture must ignore it.
	rerun.RecordedAt = recorded.RecordedAt.Add(24 * time.Hour)

	// Saved into a copy, so a failure cannot disturb the record itself.
	work := t.TempDir()
	before := copyFixtures(t, goldenDir, work)

	changed, err := conformance.SaveDir(work, &rerun)
	if err != nil {
		t.Fatalf("save the golden record: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("re-recording the same answers reported %v as changed, want nothing", changed)
	}

	after := readFixtures(t, work)
	if len(after) != len(before) {
		t.Fatalf("got %d fixtures after saving, want %d", len(after), len(before))
	}
	for name, was := range before {
		now, ok := after[name]
		if !ok {
			t.Errorf("%s was removed", name)
			continue
		}
		if !bytes.Equal(was, now) {
			t.Errorf("%s was rewritten by a run that found nothing new", name)
		}
	}
}

// A cluster that answers differently is what a re-recording exists to catch,
// so the fixture that holds the case is rewritten, and only that one.
func TestRerecordingAChangedAnswerWritesItsFixture(t *testing.T) {
	recorded, err := conformance.LoadDir(goldenDir)
	if err != nil {
		t.Fatalf("load the golden record: %v", err)
	}

	rerun := *recorded
	rerun.Cases = inSuiteOrder(t, recorded.Cases)

	// One case comes back with an outcome nobody recorded.
	var group string
	for i, c := range rerun.Cases {
		if c.Group == "isolation" {
			group = c.Group
			rerun.Cases[i].Observations = []conformance.Observation{{Outcome: "ok", CommandTag: "SET"}}
			break
		}
	}
	if group == "" {
		t.Fatal("no isolation case in the record to change")
	}

	work := t.TempDir()
	copyFixtures(t, goldenDir, work)

	changed, err := conformance.SaveDir(work, &rerun)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(changed) != 1 || changed[0] != group {
		t.Fatalf("got %v changed, want only %q", changed, group)
	}
}

// inSuiteOrder puts recorded cases back into the order a run produces them in.
// LoadDir sorts by name so that two records can be compared; a run appends in
// suite order, and that is the order the fixtures are written in.
func inSuiteOrder(t *testing.T, recorded []conformance.RecordedCase) []conformance.RecordedCase {
	t.Helper()

	byName := make(map[string]conformance.RecordedCase, len(recorded))
	for _, c := range recorded {
		byName[c.Name] = c
	}

	ordered := make([]conformance.RecordedCase, 0, len(recorded))
	for _, c := range conformance.DefaultSuite().Cases {
		if got, ok := byName[c.Name]; ok {
			ordered = append(ordered, got)
			delete(byName, c.Name)
		}
	}
	// Anything the suite no longer defines is stale in the record; keep it so
	// the comparison reports it rather than silently dropping it.
	for _, c := range recorded {
		if _, left := byName[c.Name]; left {
			ordered = append(ordered, c)
		}
	}
	return ordered
}

// copyFixtures copies every fixture in src into dst and returns their contents.
func copyFixtures(t *testing.T, src, dst string) map[string][]byte {
	t.Helper()
	contents := readFixtures(t, src)
	if len(contents) == 0 {
		t.Fatalf("no fixtures in %s", src)
	}
	for name, data := range contents {
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil {
			t.Fatalf("copy %s: %v", name, err)
		}
	}
	return contents
}

func readFixtures(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	contents := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		contents[filepath.Base(path)] = data
	}
	return contents
}

// TestBacklogHoldsOnlyOpenQuestions keeps the backlog group meaning what its
// name says. A case goes there to ask a question no recording has answered, and
// moves to the group it belongs to by subject once one has. Without this the
// group silently becomes a pile of everything ever asked, and reads as a list of
// what the emulator does not support -- which is not what it is.
func TestBacklogHoldsOnlyOpenQuestions(t *testing.T) {
	recorded, err := conformance.LoadDir(goldenDir)
	if err != nil {
		t.Fatalf("load the golden record: %v", err)
	}
	answered := make(map[string]bool, len(recorded.Cases))
	for _, c := range recorded.Cases {
		answered[c.Name] = true
	}

	open := 0
	for _, c := range conformance.DefaultSuite().Cases {
		if c.Group != "backlog" {
			continue
		}
		open++
		if answered[c.Name] {
			t.Errorf("%s is in the backlog group but the record answers it; move it to the group it belongs to by subject", c.Name)
		}
	}
	t.Logf("%d open question(s) in the backlog group", open)
}
