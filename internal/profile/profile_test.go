package profile

import (
	"strings"
	"testing"
)

const sample = `
- id: fact.role
  kind: employment
  text: "Senior Backend Engineer at Nordwind Logistik since 2021"
  verbatim: true
- id: fact.tenure
  kind: employment
  text: "Eleven years of backend development"
  verbatim: false
  until: 2027
- id: fact.degree
  kind: education
  text: "B.Sc. Informatik, 2014"
  verbatim: true
`

func load(t *testing.T) *Profile {
	t.Helper()
	p, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestParseIndexesByID(t *testing.T) {
	p := load(t)
	if len(p.Facts) != 3 {
		t.Fatalf("got %d facts, want 3", len(p.Facts))
	}
	f, ok := p.Get("fact.tenure")
	if !ok {
		t.Fatal("fact.tenure not indexed")
	}
	if f.Verbatim {
		t.Error("a fact marked not verbatim came back verbatim")
	}
}

func TestRejectsWhatCannotBeCited(t *testing.T) {
	cases := map[string]string{
		"no id":        "- kind: employment\n  text: \"x\"\n",
		"no text":      "- id: a\n  kind: employment\n  text: \"  \"\n",
		"unknown kind": "- id: a\n  kind: rumour\n  text: \"x\"\n",
		"duplicate id": "- id: a\n  kind: personal\n  text: \"x\"\n- id: a\n  kind: personal\n  text: \"y\"\n",
		"empty":        "[]",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// A duplicate id is rejected rather than merged because the fact lock
// records ids: two facts under one id make a cited claim ambiguous.
func TestDuplicateIDNamesTheOffender(t *testing.T) {
	_, err := Parse([]byte("- id: fact.x\n  kind: personal\n  text: \"a\"\n- id: fact.x\n  kind: personal\n  text: \"b\"\n"))
	if err == nil || !strings.Contains(err.Error(), "fact.x") {
		t.Errorf("got %v, want an error naming fact.x", err)
	}
}

// A claim counted in years is true for a while and then quietly is not.
func TestStaleFactsAreReported(t *testing.T) {
	p := load(t)

	if got := p.Stale(2026); len(got) != 0 {
		t.Errorf("nothing is stale in 2026, got %v", got)
	}
	got := p.Stale(2028)
	if len(got) != 1 || got[0].ID != "fact.tenure" {
		t.Errorf("got %v, want fact.tenure stale in 2028", got)
	}
}

func TestIDsCoversEveryFact(t *testing.T) {
	p := load(t)
	if len(p.IDs()) != len(p.Facts) {
		t.Errorf("got %d ids for %d facts", len(p.IDs()), len(p.Facts))
	}
}

// The committed example documents the format, so it has to stay loadable.
func TestPublishedExampleParses(t *testing.T) {
	p, err := Load("../../profile.example.yaml")
	if err != nil {
		t.Fatalf("the committed example does not parse: %v", err)
	}
	if len(p.Facts) == 0 {
		t.Error("the example carries no facts")
	}
}

// The private profile is the one that matters; skipped where absent.
func TestPrivateProfileParses(t *testing.T) {
	p, err := Load("../../profile.yaml")
	if err != nil {
		t.Skipf("no private profile: %v", err)
	}
	t.Logf("facts: %d", len(p.Facts))
	if stale := p.Stale(2026); len(stale) != 0 {
		for _, f := range stale {
			t.Errorf("%s is past its `until` year and should not be cited: %q", f.ID, f.Text)
		}
	}
}
