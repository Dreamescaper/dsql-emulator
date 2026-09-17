package conformance

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Difference is one field where the emulator disagreed with the golden record.
type Difference struct {
	Case string
	// Session is the concurrent session index; 0 for single-session cases.
	Session  int
	Steps    []string
	Step     int
	Field    string
	Golden   string
	Emulator string
	// Advisory differences are recorded and reported but not treated as
	// failures. Server wording drifts, so messages are advisory by default.
	Advisory bool
	// KnownGap carries the case's accepted-divergence note, if any.
	KnownGap string
}

func (d Difference) String() string {
	kind := "mismatch"
	if d.Advisory {
		kind = "note"
	}
	where := ""
	if d.Session > 0 {
		where = fmt.Sprintf(" session %d", d.Session)
	}
	sql := ""
	if d.Step < len(d.Steps) {
		sql = d.Steps[d.Step]
	}
	return fmt.Sprintf("%s:%s %s step %d (%s): golden=%s emulator=%s [%s]",
		d.Case, where, d.Field, d.Step, sql, d.Golden, d.Emulator, kind)
}

// Compare checks one recorded case against the emulator's replay of it. The
// emulator's copy supplies step text and suite metadata, so changes to the
// suite take effect without re-recording.
func Compare(golden, emulated RecordedCase) []Difference {
	if len(golden.SessionResults) > 0 || len(emulated.SessionResults) > 0 {
		return compareSessions(golden, emulated)
	}
	return compareObservations(golden, emulated, 0, emulated.Steps, golden.Observations, emulated.Observations)
}

func compareSessions(golden, emulated RecordedCase) []Difference {
	if len(golden.SessionResults) != len(emulated.SessionResults) {
		return []Difference{{
			Case:     golden.Name,
			Field:    "session_count",
			Golden:   fmt.Sprint(len(golden.SessionResults)),
			Emulator: fmt.Sprint(len(emulated.SessionResults)),
		}}
	}

	if emulated.ConflictRace || golden.ConflictRace {
		return compareConflictRace(golden, emulated)
	}

	var diffs []Difference
	for i := range golden.SessionResults {
		var steps []string
		if i < len(emulated.Case.Sessions) {
			steps = emulated.Case.Sessions[i]
		}
		diffs = append(diffs, compareObservations(golden, emulated, i, steps,
			golden.SessionResults[i], emulated.SessionResults[i])...)
	}
	return diffs
}

// compareConflictRace compares a case whose loser is decided by a race. Every
// step before the last is compared as usual, because Aurora DSQL lets the
// losing transaction's statements succeed too. The last step of each session is
// the COMMIT the conflict surfaces at, and what is asserted there is that as
// many transactions lost as the record shows, with the SQLSTATE it carries --
// not which of them lost, which neither system decides the same way twice.
func compareConflictRace(golden, emulated RecordedCase) []Difference {
	var diffs []Difference
	var goldenLost, emulatedLost []Observation

	for i := range golden.SessionResults {
		want, got := golden.SessionResults[i], emulated.SessionResults[i]
		var steps []string
		if i < len(emulated.Case.Sessions) {
			steps = emulated.Case.Sessions[i]
		}
		if len(want) != len(got) || len(want) == 0 {
			diffs = append(diffs, compareObservations(golden, emulated, i, steps, want, got)...)
			continue
		}

		diffs = append(diffs, compareObservations(golden, emulated, i, steps,
			want[:len(want)-1], got[:len(got)-1])...)

		if last := want[len(want)-1]; last.Outcome == "error" {
			goldenLost = append(goldenLost, last)
		}
		if last := got[len(got)-1]; last.Outcome == "error" {
			emulatedLost = append(emulatedLost, last)
		}
	}

	add := func(field, want, got string) {
		diffs = append(diffs, Difference{
			Case:     golden.Name,
			Field:    field,
			Golden:   want,
			Emulator: got,
			KnownGap: emulated.KnownGap,
		})
	}

	if len(goldenLost) != len(emulatedLost) {
		add("conflict_losers", fmt.Sprint(len(goldenLost)), fmt.Sprint(len(emulatedLost)))
		return diffs
	}
	for i, got := range emulatedLost {
		if want := goldenLost[i]; want.SQLState != got.SQLState {
			add("sqlstate", want.SQLState, got.SQLState)
		}
	}
	return diffs
}

func compareObservations(golden, emulated RecordedCase, session int, steps []string, want, got []Observation) []Difference {
	var diffs []Difference

	add := func(i int, field, wantValue, gotValue string, advisory bool) {
		diffs = append(diffs, Difference{
			Case:     golden.Name,
			Session:  session,
			Steps:    steps,
			Step:     i,
			Field:    field,
			Golden:   wantValue,
			Emulator: gotValue,
			Advisory: advisory,
			KnownGap: emulated.KnownGap,
		})
	}

	if len(want) != len(got) {
		add(0, "step_count", fmt.Sprint(len(want)), fmt.Sprint(len(got)), false)
		return diffs
	}

	for i := range want {
		w, g := want[i], got[i]

		if w.Outcome != g.Outcome {
			add(i, "outcome", w.Outcome, g.Outcome, false)
			add(i, "detail", brief(w), brief(g), true)
			continue
		}

		if w.Outcome == "error" {
			if w.SQLState != "" && w.SQLState != g.SQLState {
				add(i, "sqlstate", w.SQLState, g.SQLState, false)
			}
			if w.Message != g.Message {
				add(i, "message", w.Message, g.Message, true)
			}
			continue
		}

		// A SELECT tag carries the row count, so when the rows are ignored the
		// tag that counts them is noise too.
		if w.CommandTag != g.CommandTag && !(emulated.IgnoreRows && isRowCountTag(w.CommandTag)) {
			add(i, "command_tag", w.CommandTag, g.CommandTag, false)
		}
		if !equalStrings(w.Columns, g.Columns) {
			add(i, "columns", join(w.Columns), join(g.Columns), false)
		}
		if !emulated.IgnoreRows && rows(w.Rows) != rows(g.Rows) {
			add(i, "rows", rows(w.Rows), rows(g.Rows), false)
		}
	}

	return diffs
}

// CompareSuites matches cases by name and reports every difference. What is
// replayed is the suite's decision, not the record's: a case the suite now runs
// is compared against what was recorded for it, even if it was record-only when
// the record was made.
func CompareSuites(golden, emulated *Golden) []Difference {
	index := make(map[string]RecordedCase, len(emulated.Cases))
	for _, c := range emulated.Cases {
		index[c.Name] = c
	}

	var diffs []Difference
	for _, want := range golden.Cases {
		got, ok := index[want.Name]
		if !ok {
			// The suite declined to replay it, which the record expected.
			if want.RecordOnly {
				continue
			}
			diffs = append(diffs, Difference{
				Case:     want.Name,
				Field:    "missing",
				Golden:   "present",
				Emulator: "absent",
			})
			continue
		}
		diffs = append(diffs, Compare(want, got)...)
	}
	return diffs
}

// Unrecorded lists cases the emulator ran that the golden record does not
// cover, so probes added since the last baseline are not silently unverified.
func Unrecorded(golden, emulated *Golden) []string {
	recorded := make(map[string]bool, len(golden.Cases))
	for _, c := range golden.Cases {
		recorded[c.Name] = true
	}

	var names []string
	for _, c := range emulated.Cases {
		if !recorded[c.Name] {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	return names
}

// Failures filters out advisory differences and cases marked as known gaps.
func Failures(diffs []Difference) []Difference {
	var out []Difference
	for _, d := range diffs {
		if !d.Advisory && d.KnownGap == "" {
			out = append(out, d)
		}
	}
	return out
}

// isRowCountTag reports whether a command tag is just "SELECT <n>".
func isRowCountTag(tag string) bool {
	rest, ok := strings.CutPrefix(tag, "SELECT ")
	if !ok || rest == "" {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
}

func brief(o Observation) string {
	if o.Outcome == "error" {
		return "error " + o.SQLState + " " + o.Message
	}
	return "ok " + o.CommandTag
}

func rows(r [][]string) string {
	data, err := json.Marshal(r)
	if err != nil {
		return "<unserializable>"
	}
	return string(data)
}

func join(s []string) string {
	data, err := json.Marshal(s)
	if err != nil {
		return "<unserializable>"
	}
	return string(data)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
