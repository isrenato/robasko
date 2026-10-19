package inventory

import (
	"strings"
	"testing"
)

const sample = `
- id: go
  kind: language
  aliases: [go, golang, "go lang"]
  level: production
  evidence: "shipped it"
  last_used_year: 2026
- id: python
  kind: language
  aliases: [python]
  level: managed
  evidence: "led the team, did not write the code"
- id: kafka
  kind: framework
  aliases: [kafka]
  level: none
  evidence: ""
`

func TestCoverageTableMatchesTheSpec(t *testing.T) {
	want := map[Level]float64{
		Production: 1.0, Used: 0.8, Managed: 0.4, Learning: 0.2, None: 0.0,
	}
	for level, c := range want {
		if got := level.Coverage(); got != c {
			t.Errorf("%s: got %.2f, want %.2f", level, got, c)
		}
	}
}

// `managed` exists because leading people who write the code is real and is
// not the same as writing it. Collapsing it into `used` or `none` would lose
// the distinction the inventory is for.
func TestManagedSitsBetweenUsedAndLearning(t *testing.T) {
	if !(Learning.Coverage() < Managed.Coverage() && Managed.Coverage() < Used.Coverage()) {
		t.Errorf("managed %.2f is out of order", Managed.Coverage())
	}
}

func TestUnknownLevelCoversNothing(t *testing.T) {
	if got := Level("expert").Coverage(); got != 0 {
		t.Errorf("got %.2f, want 0", got)
	}
}

func TestParseIndexesByID(t *testing.T) {
	inv, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(inv.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(inv.Entries))
	}
	e, ok := inv.Get("python")
	if !ok {
		t.Fatal("python not indexed")
	}
	if e.Level.Coverage() != 0.4 {
		t.Errorf("coverage %.2f, want 0.40", e.Level.Coverage())
	}
}

func TestRejectsUnknownLevel(t *testing.T) {
	_, err := Parse([]byte("- id: a\n  level: wizard\n"))
	if err == nil || !strings.Contains(err.Error(), "wizard") {
		t.Errorf("got %v, want a complaint naming the level", err)
	}
}

func TestRejectsDuplicateAndMissingIDs(t *testing.T) {
	if _, err := Parse([]byte("- id: a\n  level: none\n- id: a\n  level: none\n")); err == nil {
		t.Error("expected a duplicate id to be rejected")
	}
	if _, err := Parse([]byte("- level: none\n")); err == nil {
		t.Error("expected a missing id to be rejected")
	}
}

func TestRejectsEmptyInventory(t *testing.T) {
	if _, err := Parse([]byte("[]")); err == nil {
		t.Error("expected an empty inventory to be rejected")
	}
}

// An entry is searchable by its id as well as its aliases, and the list is
// lowercased and deduplicated so matching does not depend on spelling.
func TestTermsIncludeIDAndAliasesWithoutDuplicates(t *testing.T) {
	inv, _ := Parse([]byte(sample))
	e, _ := inv.Get("go")

	terms := e.Terms()
	want := map[string]bool{"go": true, "golang": true, "go lang": true}
	if len(terms) != len(want) {
		t.Fatalf("got %v, want %d distinct terms", terms, len(want))
	}
	for _, term := range terms {
		if !want[term] {
			t.Errorf("unexpected term %q", term)
		}
	}
}

// The example file is published as the format's documentation, so it has to
// stay parseable.
func TestPublishedExampleParses(t *testing.T) {
	inv, err := Load("../../inventory.example.yaml")
	if err != nil {
		t.Fatalf("the committed example does not parse: %v", err)
	}
	var levels = map[Level]int{}
	for _, e := range inv.Entries {
		levels[e.Level]++
	}
	for _, l := range []Level{Production, Used, Managed, Learning, None} {
		if levels[l] == 0 {
			t.Errorf("the example shows no %q entry", l)
		}
	}
}
