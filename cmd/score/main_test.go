package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/llm"
	"github.com/isrenato/robasko/internal/store"
)

const testInventory = `
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
- id: kafka
  kind: framework
  aliases: [kafka]
  level: none
  evidence: ""
`

func testInv(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.Parse([]byte(testInventory))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return inv
}

// stagedClient answers the two calls of the pipeline differently, by
// looking at which one it is being asked. Keying on the exchange hash
// would tie the test to the exact wording of two prompts that are meant
// to be edited.
type stagedClient struct {
	extract, mapping string
	extractErr       error
	mappingErr       error
	calls            int
}

func (s *stagedClient) Structured(_ context.Context, req llm.Request) (*llm.Response, error) {
	s.calls++
	if strings.HasPrefix(req.Prompt, "Advertisement:") {
		if s.extractErr != nil {
			return nil, s.extractErr
		}
		return &llm.Response{JSON: json.RawMessage(s.extract), Model: "fake",
			InputTokens: 100, OutputTokens: 20, CacheRead: 5}, nil
	}
	if s.mappingErr != nil {
		return nil, s.mappingErr
	}
	return &llm.Response{JSON: json.RawMessage(s.mapping), Model: "fake",
		InputTokens: 50, OutputTokens: 10, CacheRead: 7}, nil
}

func vacancy() store.StoredVacancy {
	return store.StoredVacancy{
		ID: "v-1", Title: "Backend Engineer", Company: "Testfirma",
		City: "Berlin", Remote: "remote",
		Text: "Wir suchen eine Entwicklerin mit Erfahrung in Go.",
	}
}

func TestAnalyseRunsTheWholePipeline(t *testing.T) {
	c := &stagedClient{
		extract: `{"requirements":[
			{"text":"Erfahrung in Go","kind":"must","category":"language"},
			{"text":"Idealerweise Kubernetes","kind":"nice","category":"cloud"},
			{"text":"Shopware","kind":"must","category":"framework"}
		]}`,
		mapping: `{"mappings":[
			{"index":0,"inventory_ids":["go"]},
			{"index":1,"inventory_ids":["kubernetes"]},
			{"index":2,"inventory_ids":[]}
		]}`,
	}

	got, usage, err := analyse(context.Background(), c, testInv(t), vacancy())
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if c.calls != 2 {
		t.Errorf("made %d model calls, want 2", c.calls)
	}

	if len(got.Requirements) != 3 {
		t.Fatalf("got %d requirements, want 3", len(got.Requirements))
	}
	if got.Requirements[0].Coverage != 1.0 {
		t.Errorf("Go should be fully covered, got %.2f", got.Requirements[0].Coverage)
	}
	// The unmapped must-have is what keeps the score honest.
	if got.Requirements[2].Coverage != 0 {
		t.Errorf("an unmapped requirement carries %.2f", got.Requirements[2].Coverage)
	}
	if len(got.Result.MustGaps) != 1 {
		t.Errorf("got %d must-gaps, want 1", len(got.Result.MustGaps))
	}
	// must coverage (1.0 + 0)/2 = 0.5 -> capped at 70 by the uncovered must
	if got.Result.Score > 70 {
		t.Errorf("score %.1f should be capped by the uncovered must-have", got.Result.Score)
	}

	// Usage has to cover both calls, or a cost report under-reports.
	if usage.InputTokens != 150 || usage.OutputTokens != 30 || usage.CacheRead != 12 {
		t.Errorf("usage %+v does not add up across both calls", usage)
	}
}

// The category comes from extraction and is never seen by mapping, so it
// has to be carried across rather than re-derived.
func TestCategoriesSurviveMapping(t *testing.T) {
	c := &stagedClient{
		extract: `{"requirements":[
			{"text":"Go","kind":"must","category":"language"},
			{"text":"AWS","kind":"nice","category":"cloud"}
		]}`,
		mapping: `{"mappings":[{"index":0,"inventory_ids":["go"]}]}`,
	}

	got, _, err := analyse(context.Background(), c, testInv(t), vacancy())
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if len(got.Categories) != 2 {
		t.Fatalf("got %d categories for %d requirements", len(got.Categories), len(got.Requirements))
	}
	if got.Categories[0] != "language" || got.Categories[1] != "cloud" {
		t.Errorf("categories not carried: %v", got.Categories)
	}
}

// The digest is what ties a stored score to the inventory it was computed
// against; without it a score cannot be told apart from a stale one.
func TestInventoryDigestIsRecorded(t *testing.T) {
	inv := testInv(t)
	c := &stagedClient{
		extract: `{"requirements":[{"text":"Go","kind":"must","category":"language"}]}`,
		mapping: `{"mappings":[{"index":0,"inventory_ids":["go"]}]}`,
	}

	got, _, err := analyse(context.Background(), c, inv, vacancy())
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if got.InventoryDigest != inv.Digest() {
		t.Errorf("digest %q, want %q", got.InventoryDigest, inv.Digest())
	}
	if got.VacancyID != "v-1" {
		t.Errorf("vacancy id %q", got.VacancyID)
	}
}

// Which of the two calls failed decides what to do about it, so the error
// has to say.
func TestFailuresNameTheStageTheyCameFrom(t *testing.T) {
	cases := map[string]struct {
		client *stagedClient
		want   string
	}{
		"extraction": {&stagedClient{extractErr: errors.New("boom")}, "extracting"},
		"mapping": {&stagedClient{
			extract:    `{"requirements":[{"text":"Go","kind":"must","category":"language"}]}`,
			mappingErr: errors.New("boom"),
		}, "mapping"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := analyse(context.Background(), tc.client, testInv(t), vacancy())
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// Usage is reported even when the run fails, so a batch that dies halfway
// still accounts for what it spent.
func TestUsageIsReportedOnFailure(t *testing.T) {
	c := &stagedClient{
		extract:    `{"requirements":[{"text":"Go","kind":"must","category":"language"}]}`,
		mappingErr: errors.New("boom"),
	}

	_, usage, err := analyse(context.Background(), c, testInv(t), vacancy())
	if err == nil {
		t.Fatal("expected a failure")
	}
	if usage.InputTokens != 100 {
		t.Errorf("the extraction's %d input tokens were lost: %+v", 100, usage)
	}
}

// An advertisement the model reads nothing out of is not an error. It
// scores zero and says so, rather than being skipped and silently
// re-queued on the next run.
func TestAdvertisementWithNoRequirements(t *testing.T) {
	c := &stagedClient{extract: `{"requirements":[]}`, mapping: `{"mappings":[]}`}

	got, _, err := analyse(context.Background(), c, testInv(t), vacancy())
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if len(got.Requirements) != 0 || got.Result.Score != 0 {
		t.Errorf("got %+v", got.Result)
	}
	// Mapping is not called when there is nothing to map.
	if c.calls != 1 {
		t.Errorf("made %d calls, want 1", c.calls)
	}
}

func TestTruncateKeepsItWithinWidth(t *testing.T) {
	long := strings.Repeat("a", 100)
	if got := truncate(long, 20); len([]rune(got)) != 20 {
		t.Errorf("got %d runes, want 20", len([]rune(got)))
	}
	if got := truncate("short", 20); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := truncate("a  b\n c", 20); got != "a b c" {
		t.Errorf("whitespace not collapsed: %q", got)
	}
}
