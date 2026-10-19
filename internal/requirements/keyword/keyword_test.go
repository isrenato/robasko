package keyword

import (
	"strings"
	"testing"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/scoring"
)

const inventoryYAML = `
- id: go
  kind: language
  aliases: [go, golang]
  level: production
  evidence: "shipped it"
- id: kubernetes
  kind: cloud
  aliases: [kubernetes, k8s]
  level: learning
  evidence: ""
- id: kafka
  kind: framework
  aliases: [kafka]
  level: none
  evidence: ""
- id: python
  kind: language
  aliases: [python]
  level: managed
  evidence: "led the team"
`

func load(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.Parse([]byte(inventoryYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return inv
}

func byID(reqs []scoring.Requirement, id string) (scoring.Requirement, bool) {
	for _, r := range reqs {
		for _, got := range r.IDs {
			if got == id {
				return r, true
			}
		}
	}
	return scoring.Requirement{}, false
}

func TestMentionedSkillBecomesARequirement(t *testing.T) {
	got := Extract("Mehrjährige Erfahrung mit Go erforderlich.", load(t))

	r, ok := byID(got, "go")
	if !ok {
		t.Fatalf("Go was not extracted: %+v", got)
	}
	if r.Kind != scoring.Must {
		t.Errorf("kind = %q, want must", r.Kind)
	}
	if r.Coverage != 1.0 {
		t.Errorf("coverage = %.2f, want 1.00", r.Coverage)
	}
	if !strings.Contains(r.Text, "Erfahrung mit Go") {
		t.Errorf("text is not the sentence it came from: %q", r.Text)
	}
}

// German advertisements soften preferences consistently, and the distinction
// drives the whole score: a nice-to-have carries a fifth of the weight.
func TestSoftenedLinesBecomeNiceToHave(t *testing.T) {
	inv := load(t)
	for _, line := range []string{
		"Idealerweise Kubernetes",
		"Kubernetes von Vorteil",
		"Kubernetes wünschenswert",
		"Kubernetes is a plus",
		"Kubernetes nice to have",
	} {
		got := Extract(line, inv)
		r, ok := byID(got, "kubernetes")
		if !ok {
			t.Errorf("%q: not extracted", line)
			continue
		}
		if r.Kind != scoring.Nice {
			t.Errorf("%q: kind = %q, want nice", line, r.Kind)
		}
	}
}

func TestPlainLineIsAMustHave(t *testing.T) {
	got := Extract("Sie bringen Kubernetes mit.", load(t))
	r, _ := byID(got, "kubernetes")
	if r.Kind != scoring.Must {
		t.Errorf("kind = %q, want must", r.Kind)
	}
}

// A skill demanded in one place and merely preferred in another is
// demanded; taking the softer reading would quietly inflate the score.
func TestDemandBeatsPreferenceForTheSameSkill(t *testing.T) {
	got := Extract("Kubernetes von Vorteil.\nKubernetes ist Voraussetzung.", load(t))

	if n := len(got); n != 1 {
		t.Fatalf("got %d requirements, want 1 deduplicated", n)
	}
	if got[0].Kind != scoring.Must {
		t.Errorf("kind = %q, want must", got[0].Kind)
	}
}

// "go" sits inside "Golang" but also inside "Cellagon" and "Google". Whole
// words only, or every advertising job matches a Go search.
func TestSubstringsDoNotCount(t *testing.T) {
	got := Extract("Wir sind Cellagon und suchen einen Google Ads Manager.", load(t))
	if _, ok := byID(got, "go"); ok {
		t.Errorf("matched inside a longer word: %+v", got)
	}
}

func TestUncoveredSkillIsStillExtracted(t *testing.T) {
	got := Extract("Erfahrung mit Kafka erforderlich.", load(t))

	r, ok := byID(got, "kafka")
	if !ok {
		t.Fatal("a skill held at `none` must still produce a requirement")
	}
	if r.Coverage != 0 {
		t.Errorf("coverage = %.2f, want 0", r.Coverage)
	}

	// It has to reach the score as a gap, which is what caps the result.
	res := scoring.Compute(got, scoring.Options{})
	if len(res.MustGaps) != 1 {
		t.Errorf("got %d must-gaps, want 1", len(res.MustGaps))
	}
}

func TestManagedLevelCarriesPartialCoverage(t *testing.T) {
	got := Extract("Python erforderlich.", load(t))
	r, _ := byID(got, "python")
	if r.Coverage != 0.4 {
		t.Errorf("coverage = %.2f, want 0.40 for a managed skill", r.Coverage)
	}
	if r.Evidence == "" {
		t.Error("the inventory evidence did not reach the requirement")
	}
}

func TestUnmentionedSkillsAreAbsent(t *testing.T) {
	got := Extract("Wir suchen einen Buchhalter.", load(t))
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

func TestOrderFollowsTheAdvertisement(t *testing.T) {
	got := Extract("Kafka zuerst.\nDann Python.\nZuletzt Go.", load(t))
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	want := []string{"kafka", "python", "go"}
	for i, id := range want {
		if got[i].IDs[0] != id {
			t.Errorf("position %d is %q, want %q", i, got[i].IDs[0], id)
		}
	}
}
