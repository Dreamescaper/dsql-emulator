// Package rules holds the versioned description of what Aurora DSQL supports.
// The ruleset is data, not code, because the feature matrix changes over time.
package rules

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed dsql-2026.09.yaml
var defaultRuleset []byte

// Ruleset is a versioned snapshot of Aurora DSQL's supported surface.
type Ruleset struct {
	DSQLVersion string    `yaml:"dsql_version"`
	Unsupported []Rule    `yaml:"unsupported"`
	Isolation   Isolation `yaml:"isolation"`
	Limits      Limits    `yaml:"limits"`
	OCC         OCC       `yaml:"occ"`
}

// Isolation lists the transaction isolation levels Aurora DSQL accepts. It
// reports REPEATABLE READ and rejects every other level the client asks for.
type Isolation struct {
	Supported []string `yaml:"supported"`
}

// Rule rejects a statement that matches every predicate it sets. An empty
// predicate matches anything.
type Rule struct {
	ID             string   `yaml:"id"`
	Stmt           string   `yaml:"stmt"`
	Relpersistence []string `yaml:"relpersistence"`
	ColumnType     []string `yaml:"column_type"`
	Objtype        []string `yaml:"objtype"`
	TxnKind        []string `yaml:"txn_kind"`
	// SetName matches the parameter named by SET, for example TRANSACTION.
	SetName []string `yaml:"set_name"`
	// ColumnArray matches a table that declares an array column.
	ColumnArray bool `yaml:"column_array"`
	// RemoveType matches the object kind of a DROP, and RenameType the object
	// kind of an ALTER ... RENAME.
	RemoveType []string `yaml:"remove_type"`
	RenameType []string `yaml:"rename_type"`
	// Locking matches the row-locking strengths of a SELECT.
	Locking []string `yaml:"locking"`
	// Function matches any function called anywhere in the statement, and
	// Contains any nested node by oneof name. VacuumKind separates VACUUM from
	// ANALYZE, which share a statement node.
	Function   []string `yaml:"function"`
	Contains   []string `yaml:"contains"`
	VacuumKind []string `yaml:"vacuum_kind"`
	// ShowName matches the parameter named by SHOW.
	ShowName []string `yaml:"show_name"`
	// AlterAction matches ALTER TABLE command subtypes, such as
	// AT_ValidateConstraint.
	AlterAction []string `yaml:"alter_action"`
	// AddConstraintMissingNotValid matches an ALTER TABLE that adds a CHECK or
	// FOREIGN KEY without the NOT VALID the dialect requires.
	AddConstraintMissingNotValid bool `yaml:"add_constraint_missing_not_valid"`
	// AddConstraintType matches an ALTER TABLE that adds a constraint of one of
	// these kinds, naming them as libpg_query does.
	AddConstraintType []string `yaml:"add_constraint_type"`
	// LanguageNot lists the languages a function may use; any other language
	// matches the rule.
	LanguageNot []string `yaml:"language_not"`
	// IdentityTypeNot lists the types an identity column may have, by the name
	// the parser uses internally; an identity column of any other type matches
	// the rule.
	IdentityTypeNot []string `yaml:"identity_type_not"`
	// UnlessAsync marks a rule that exists only to require the dialect's ASYNC
	// form. It does not apply to a statement that carried ASYNC, because the
	// keyword is stripped before the statement is parsed.
	UnlessAsync bool `yaml:"unless_async"`
	// SequenceCacheMin and IdentityCacheMin reject a sequence, or an identity
	// column, whose CACHE is missing or smaller than the minimum, unless the
	// value appears in CacheAllow.
	SequenceCacheMin *int   `yaml:"sequence_cache_min"`
	IdentityCacheMin *int   `yaml:"identity_cache_min"`
	CacheAllow       []int  `yaml:"cache_allow"`
	Code             string `yaml:"code"`
	Message          string `yaml:"message"`
	Since            string `yaml:"since"`
}

// Limits are the per-transaction caps enforced by the session state machine.
type Limits struct {
	DMLRowsPerTxn int `yaml:"dml_rows_per_txn"`
	TxnAgeSeconds int `yaml:"txn_age_seconds"`
}

// OCC describes conflict behavior.
type OCC struct {
	// Sources and KeyColumnsOnlyFor record which overlaps Aurora DSQL treats as
	// a conflict. Nothing reads them: the emulator delegates the decision to
	// the backend's lock manager, whose row-lock modes already draw the same
	// lines, down to a non-key update not conflicting with a referencing
	// insert. They are kept as the statement of what is being emulated.
	Sources           []string       `yaml:"sources"`
	KeyColumnsOnlyFor []string       `yaml:"key_columns_only_for"`
	Error             string         `yaml:"error"`
	SQLState          string         `yaml:"sqlstate"`
	Inject            []OCCInjection `yaml:"inject"`
	// LockTimeoutMS bounds how long the backend waits for a row lock. Aurora
	// DSQL never waits, so a wait that runs out is the evidence that two
	// transactions want the same rows, which the emulator resolves at COMMIT.
	// Zero leaves the backend waiting, which blocks where DSQL would not.
	LockTimeoutMS int `yaml:"lock_timeout_ms"`
}

// OCCInjection fails a transaction at COMMIT when it has touched one of the
// listed tables, every Nth time. With no tables it matches any transaction, and
// with no Every, or 1, it fails every commit it matches.
type OCCInjection struct {
	ID     string   `yaml:"id"`
	Tables []string `yaml:"tables"`
	Every  int      `yaml:"every"`
}

// Default returns the embedded ruleset.
func Default() (*Ruleset, error) {
	return Load(bytes.NewReader(defaultRuleset))
}

// Load reads and validates a ruleset.
func Load(r io.Reader) (*Ruleset, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var rs Ruleset
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("decode ruleset: %w", err)
	}
	if err := rs.validate(); err != nil {
		return nil, err
	}
	return &rs, nil
}

func (rs *Ruleset) validate() error {
	if rs.DSQLVersion == "" {
		return errors.New("ruleset: dsql_version is required")
	}
	if len(rs.Isolation.Supported) == 0 {
		return errors.New("ruleset: isolation.supported must list at least one level")
	}
	seen := make(map[string]bool, len(rs.Unsupported))
	for i, r := range rs.Unsupported {
		switch {
		case r.ID == "":
			return fmt.Errorf("ruleset: rule %d has no id", i)
		case seen[r.ID]:
			return fmt.Errorf("ruleset: duplicate rule id %q", r.ID)
		case r.Stmt == "":
			return fmt.Errorf("ruleset: rule %q has no stmt", r.ID)
		case r.Code == "":
			return fmt.Errorf("ruleset: rule %q has no code", r.ID)
		case r.Message == "":
			return fmt.Errorf("ruleset: rule %q has no message", r.ID)
		}
		seen[r.ID] = true
	}
	return rs.OCC.validate()
}

// validate checks the conflict settings. An injection rule that is quietly
// ignored is worse than one that is refused: the transaction it was meant to
// fail commits, and a retry loop under test never runs.
func (o OCC) validate() error {
	if o.LockTimeoutMS < 0 {
		return fmt.Errorf("ruleset: occ.lock_timeout_ms is %d; use 0 to let the backend wait", o.LockTimeoutMS)
	}

	seen := make(map[string]bool, len(o.Inject))
	for i, inj := range o.Inject {
		switch {
		case inj.ID == "":
			return fmt.Errorf("ruleset: occ injection %d has no id", i)
		case seen[inj.ID]:
			return fmt.Errorf("ruleset: duplicate occ injection id %q", inj.ID)
		case inj.Every < 0:
			return fmt.Errorf("ruleset: occ injection %q has every %d; use 1 for every commit", inj.ID, inj.Every)
		}
		for _, table := range inj.Tables {
			if strings.TrimSpace(table) == "" {
				return fmt.Errorf("ruleset: occ injection %q names an empty table", inj.ID)
			}
		}
		seen[inj.ID] = true
	}
	return nil
}
