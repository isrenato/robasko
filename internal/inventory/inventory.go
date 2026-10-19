// Package inventory loads the structured record of what the candidate can
// do, at what level, with the evidence behind it.
//
// The file is written and maintained by hand (SPEC.md §10). Nothing here
// writes to it: a model that can edit its own evidence base is a model that
// can talk itself into a qualification.
package inventory

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Level is how well a skill is actually held.
type Level string

const (
	Production Level = "production"
	Used       Level = "used"
	Managed    Level = "managed"
	Learning   Level = "learning"
	None       Level = "none"
)

// coverage is the fraction of a requirement a level satisfies.
//
// `managed` sits at 0.4 because leading people who write the code is real
// experience and is not the same as writing it.
var coverage = map[Level]float64{
	Production: 1.0,
	Used:       0.8,
	Managed:    0.4,
	Learning:   0.2,
	None:       0.0,
}

// Coverage returns the level's coverage, and 0 for anything unrecognised.
func (l Level) Coverage() float64 { return coverage[l] }

// Valid reports whether the level is one of the five.
func (l Level) Valid() bool { _, ok := coverage[l]; return ok }

// Entry is one skill.
type Entry struct {
	ID           string   `yaml:"id"`
	Kind         string   `yaml:"kind"`
	Aliases      []string `yaml:"aliases"`
	Level        Level    `yaml:"level"`
	Evidence     string   `yaml:"evidence"`
	LastUsedYear int      `yaml:"last_used_year"`
	CEFR         string   `yaml:"cefr"`
}

// Inventory is the loaded file, indexed for lookup by alias.
type Inventory struct {
	Entries []Entry
	byID    map[string]*Entry
}

// Load reads an inventory file.
func Load(path string) (*Inventory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the inventory: %w", err)
	}
	return Parse(raw)
}

// Parse reads an inventory from YAML.
func Parse(raw []byte) (*Inventory, error) {
	var entries []Entry
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parsing the inventory: %w", err)
	}

	inv := &Inventory{Entries: entries, byID: make(map[string]*Entry, len(entries))}
	for i := range inv.Entries {
		e := &inv.Entries[i]
		if e.ID == "" {
			return nil, fmt.Errorf("entry %d has no id", i+1)
		}
		if !e.Level.Valid() {
			return nil, fmt.Errorf("entry %q has an unknown level %q", e.ID, e.Level)
		}
		if _, dup := inv.byID[e.ID]; dup {
			return nil, fmt.Errorf("entry %q appears twice", e.ID)
		}
		inv.byID[e.ID] = e
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("the inventory is empty")
	}
	return inv, nil
}

// Get returns the entry with this id.
func (inv *Inventory) Get(id string) (*Entry, bool) {
	e, ok := inv.byID[id]
	return e, ok
}

// Terms returns every searchable spelling of an entry: its id and aliases.
func (e *Entry) Terms() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range append([]string{e.ID}, e.Aliases...) {
		t = strings.TrimSpace(strings.ToLower(t))
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
