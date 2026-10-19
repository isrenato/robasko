package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/scoring"
)

const mapInventory = `
- id: go
  kind: language
  aliases: [go, golang]
  level: production
  evidence: "shipped it"
- id: kubernetes
  kind: cloud
  aliases: [kubernetes, k8s]
  level: learning
  evidence: "studying"
- id: postgres
  kind: db
  aliases: [postgres, postgresql]
  level: used
  evidence: "built on it"
- id: kafka
  kind: framework
  aliases: [kafka]
  level: none
  evidence: ""
`

func mapInv(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.Parse([]byte(mapInventory))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return inv
}

func reqs(items ...[2]string) []Requirement {
	out := make([]Requirement, 0, len(items))
	for _, it := range items {
		out = append(out, Requirement{Text: it[0], Kind: it[1], Category: "other"})
	}
	return out
}

func TestMappingAttachesIDsAndCoverage(t *testing.T) {
	f := fakeWith(t, `{"mappings":[
		{"index":0,"inventory_ids":["go"]},
		{"index":1,"inventory_ids":["kubernetes"]}
	]}`)

	got, _, err := MapRequirements(context.Background(), f,
		reqs([2]string{"Erfahrung mit Go", KindMust}, [2]string{"Idealerweise Kubernetes", KindNice}),
		mapInv(t))
	if err != nil {
		t.Fatalf("MapRequirements: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].Coverage != 1.0 || got[0].Kind != scoring.Must {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Coverage != 0.2 {
		t.Errorf("a learning entry should carry 0.2, got %.2f", got[1].Coverage)
	}
	if got[0].Evidence == "" {
		t.Error("the inventory evidence did not reach the requirement")
	}
}

// SPEC.md §11: a requirement mapped to several ids takes the highest
// coverage among them.
func TestSeveralIDsTakeTheHighestCoverage(t *testing.T) {
	f := fakeWith(t, `{"mappings":[{"index":0,"inventory_ids":["kubernetes","go","postgres"]}]}`)

	got, _, err := MapRequirements(context.Background(), f,
		reqs([2]string{"Go, Kubernetes und Postgres", KindMust}), mapInv(t))
	if err != nil {
		t.Fatalf("MapRequirements: %v", err)
	}
	if got[0].Coverage != 1.0 {
		t.Errorf("got %.2f, want 1.00 from the strongest of the three", got[0].Coverage)
	}
	if len(got[0].IDs) != 3 {
		t.Errorf("all three ids should be recorded: %v", got[0].IDs)
	}
}

// A demand the inventory has no word for must score zero rather than be
// matched loosely. This is the case that keeps a score honest.
func TestUnmappedRequirementScoresZero(t *testing.T) {
	f := fakeWith(t, `{"mappings":[{"index":0,"inventory_ids":[]}]}`)

	got, _, err := MapRequirements(context.Background(), f,
		reqs([2]string{"Erfahrung mit Shopware", KindMust}), mapInv(t))
	if err != nil {
		t.Fatalf("MapRequirements: %v", err)
	}
	if got[0].Coverage != 0 || len(got[0].IDs) != 0 {
		t.Errorf("got %+v, want no ids and no coverage", got[0])
	}

	// And it has to reach the score as a gap.
	res := scoring.Compute(got, scoring.Options{})
	if len(res.MustGaps) != 1 {
		t.Errorf("got %d must-gaps, want 1", len(res.MustGaps))
	}
}

// A `none` entry is mapped - the inventory names the skill - but carries
// no coverage, which is how a known gap differs from an unknown one.
func TestKnownGapIsMappedButUncovered(t *testing.T) {
	f := fakeWith(t, `{"mappings":[{"index":0,"inventory_ids":["kafka"]}]}`)

	got, _, _ := MapRequirements(context.Background(), f,
		reqs([2]string{"Kafka erforderlich", KindMust}), mapInv(t))
	if got[0].Coverage != 0 {
		t.Errorf("a `none` entry carries %.2f, want 0", got[0].Coverage)
	}
	if len(got[0].IDs) != 1 {
		t.Errorf("the entry should still be recorded: %v", got[0].IDs)
	}
}

// A requirement the model simply omitted is unmapped, not missing: the
// scoring engine must still see it, or the denominator shrinks and the
// score rises for the requirements the model failed to answer.
func TestOmittedRequirementIsStillScored(t *testing.T) {
	f := fakeWith(t, `{"mappings":[{"index":0,"inventory_ids":["go"]}]}`)

	got, _, err := MapRequirements(context.Background(), f,
		reqs([2]string{"Go", KindMust}, [2]string{"Something else", KindMust}), mapInv(t))
	if err != nil {
		t.Fatalf("MapRequirements: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d requirements back, want both", len(got))
	}
	if got[1].Coverage != 0 {
		t.Errorf("the omitted requirement got coverage %.2f", got[1].Coverage)
	}
}

func TestOutOfRangeIndexIsRejected(t *testing.T) {
	f := fakeWith(t, `{"mappings":[{"index":7,"inventory_ids":["go"]}]}`)

	if _, _, err := MapRequirements(context.Background(), f,
		reqs([2]string{"Go", KindMust}), mapInv(t)); err == nil {
		t.Error("a mapping pointing past the list was accepted")
	}
}

func TestUnknownIDIsRejected(t *testing.T) {
	f := fakeWith(t, `{"mappings":[{"index":0,"inventory_ids":["rust"]}]}`)

	_, _, err := MapRequirements(context.Background(), f,
		reqs([2]string{"Rust", KindMust}), mapInv(t))
	if err == nil || !strings.Contains(err.Error(), "rust") {
		t.Errorf("got %v, want an error naming the unknown id", err)
	}
}

// The enum is what makes an invented id unrepresentable rather than merely
// rejected after the fact.
func TestSchemaEnumIsExactlyTheInventory(t *testing.T) {
	inv := mapInv(t)
	s := MapSchema(inv)

	items := s["properties"].(map[string]any)["mappings"].(map[string]any)["items"].(map[string]any)
	ids := items["properties"].(map[string]any)["inventory_ids"].(map[string]any)["items"].(map[string]any)["enum"].([]string)

	if len(ids) != len(inv.Entries) {
		t.Fatalf("enum has %d ids, inventory has %d", len(ids), len(inv.Entries))
	}
	for _, id := range ids {
		if _, ok := inv.Get(id); !ok {
			t.Errorf("enum carries %q, which is not in the inventory", id)
		}
	}
	if items["additionalProperties"] != false {
		t.Error("a mapping accepts unknown properties")
	}
}

// Levels are withheld on purpose: a model that knows which entries are
// strong can map generously towards them.
func TestInventoryLevelsAreNotShownToTheModel(t *testing.T) {
	f := fakeWith(t, `{"mappings":[]}`)

	if _, _, err := MapRequirements(context.Background(), f,
		reqs([2]string{"Go", KindMust}), mapInv(t)); err != nil {
		t.Fatalf("MapRequirements: %v", err)
	}
	system := f.Calls[0].System

	for _, leak := range []string{"production", "learning", "shipped it", "built on it"} {
		if strings.Contains(system, leak) {
			t.Errorf("the prompt leaks %q", leak)
		}
	}
	// The ids and aliases must be there, or the model cannot match at all.
	for _, needed := range []string{"go", "golang", "kubernetes", "k8s"} {
		if !strings.Contains(system, needed) {
			t.Errorf("the prompt omits %q", needed)
		}
	}
}

// The inventory sits in the system prompt, so the cached prefix holds
// across every vacancy scored against one inventory and breaks exactly
// when the inventory changes.
func TestSystemPromptIsStableAcrossVacancies(t *testing.T) {
	f := fakeWith(t, `{"mappings":[]}`)
	inv := mapInv(t)

	for _, text := range []string{"first", "second"} {
		if _, _, err := MapRequirements(context.Background(), f,
			reqs([2]string{text, KindMust}), inv); err != nil {
			t.Fatalf("MapRequirements: %v", err)
		}
	}
	if f.Calls[0].System != f.Calls[1].System {
		t.Error("the system prompt differed between vacancies")
	}
	if f.Calls[0].Prompt == f.Calls[1].Prompt {
		t.Error("the requirements did not reach the user prompt")
	}
}

func TestNoRequirementsMakesNoCall(t *testing.T) {
	f := fakeWith(t, `{"mappings":[]}`)

	got, _, err := MapRequirements(context.Background(), f, nil, mapInv(t))
	if err != nil || got != nil {
		t.Errorf("got %v, %v", got, err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("the model was called %d times for nothing", len(f.Calls))
	}
}

func TestMappingNeedsAnInventory(t *testing.T) {
	f := fakeWith(t, `{"mappings":[]}`)
	if _, _, err := MapRequirements(context.Background(), f, reqs([2]string{"Go", KindMust}), nil); err == nil {
		t.Error("mapping proceeded without an inventory")
	}
}

var _ = json.Marshal
