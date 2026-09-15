package conformance

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Difference is one field where the emulator disagreed with the golden record.
type Difference struct {
	Case     string
	Step     int
	SQL      string
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
	return fmt.Sprintf("%s: %s step %d (%s): golden=%s emulator=%s [%s]",
		d.Case, d.Field, d.Step, d.SQL, d.Golden, d.Emulator, kind)
}

// Compare checks one recorded case against the emulator's replay of it. The
// emulator's copy supplies step text and suite metadata, so changes to the
// suite take effect without re-recording the baseline.
func Compare(golden, emulated RecordedCase) []Difference {
	var diffs []Difference

	step := func(i int) string {
		if i < len(emulated.Steps) {
			return emulated.Steps[i]
		}
		return ""
	}
	add := func(i int, field, want, got string, advisory bool) {
		diffs = append(diffs, Difference{
			Case:     golden.Name,
			Step:     i,
			SQL:      step(i),
			Field:    field,
			Golden:   want,
			Emulator: got,
			Advisory: advisory,
			KnownGap: emulated.KnownGap,
		})
	}

	if len(golden.Observations) != len(emulated.Observations) {
		add(0, "step_count", fmt.Sprint(len(golden.Observations)), fmt.Sprint(len(emulated.Observations)), false)
		return diffs
	}

	for i := range golden.Observations {
		want, got := golden.Observations[i], emulated.Observations[i]

		if want.Outcome != got.Outcome {
			add(i, "outcome", want.Outcome, got.Outcome, false)
			add(i, "detail", brief(want), brief(got), true)
			continue
		}

		if want.Outcome == "error" {
			if want.SQLState != "" && want.SQLState != got.SQLState {
				add(i, "sqlstate", want.SQLState, got.SQLState, false)
			}
			if want.Message != got.Message {
				add(i, "message", want.Message, got.Message, true)
			}
			continue
		}

		if want.CommandTag != got.CommandTag {
			add(i, "command_tag", want.CommandTag, got.CommandTag, false)
		}
		if emulated.IgnoreRows {
			continue
		}
		if !equalStrings(want.Columns, got.Columns) {
			add(i, "columns", join(want.Columns), join(got.Columns), false)
		}
		if rows(want.Rows) != rows(got.Rows) {
			add(i, "rows", rows(want.Rows), rows(got.Rows), false)
		}
	}

	return diffs
}

// CompareSuites matches cases by name and reports every difference.
func CompareSuites(golden, emulated *Golden) []Difference {
	index := make(map[string]RecordedCase, len(emulated.Cases))
	for _, c := range emulated.Cases {
		index[c.Name] = c
	}

	var diffs []Difference
	for _, want := range golden.Cases {
		got, ok := index[want.Name]
		if !ok {
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
