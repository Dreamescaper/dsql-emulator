// Package rules holds the versioned description of what Aurora DSQL supports.
// The ruleset is data, not code, because the feature matrix changes over time.
package rules

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"

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
	// LanguageNot lists the languages a function may use; any other language
	// matches the rule.
	LanguageNot []string `yaml:"language_not"`
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
	Sources           []string       `yaml:"sources"`
	KeyColumnsOnlyFor []string       `yaml:"key_columns_only_for"`
	Error             string         `yaml:"error"`
	SQLState          string         `yaml:"sqlstate"`
	Inject            []OCCInjection `yaml:"inject"`
}

// OCCInjection fails a transaction at COMMIT when it has touched one of the
// listed tables, every Nth time. With no tables it matches any transaction.
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
	return nil
}
