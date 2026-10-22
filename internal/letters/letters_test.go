package letters

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/llm"
	"github.com/isrenato/robasko/internal/profile"
	"github.com/isrenato/robasko/internal/scoring"
)

const invYAML = `
- id: go
  kind: language
  aliases: [go, golang]
  level: production
  evidence: "Serverless functions on Lambda"
- id: python
  kind: language
  aliases: [python]
  level: managed
  evidence: "Led the department; did not write the code"
- id: kafka
  kind: framework
  aliases: [kafka]
  level: none
  evidence: ""
`

const profYAML = `
- id: fact.role
  kind: employment
  text: "Senior Backend Engineer at Nordwind Logistik since 2021"
  verbatim: true
- id: fact.latency
  kind: achievement
  text: "Cut delivery status latency from hours to seconds"
  verbatim: true
`

func sources(t *testing.T) Sources {
	t.Helper()
	inv, err := inventory.Parse([]byte(invYAML))
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	p, err := profile.Parse([]byte(profYAML))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	return Sources{Inventory: inv, Profile: p}
}

func passing() scoring.Result {
	return scoring.Result{Score: 86, MustCoverage: 1, MustCount: 3, NiceCoverage: 0.5, NiceCount: 2}
}

func fake(t *testing.T, reply string) *llm.Fake {
	t.Helper()
	f := llm.NewFake()
	f.Reply("only", json.RawMessage(reply))
	return f
}

const goodDraft = `{
 "salutation": "Sehr geehrte Frau Beispiel,",
 "paragraphs": [
   {"id":"opening","sentences":[
     {"text":"Sie suchen jemanden für Go im Backend.","sources":["go"]}]},
   {"id":"evidence","sentences":[
     {"text":"Ich schreibe Go für serverlose Funktionen auf Lambda.","sources":["go"]},
     {"text":"Dabei habe ich die Latenz von Stunden auf Sekunden gesenkt.","sources":["fact.latency"]}]}
 ],
 "closing": "Mit freundlichen Grüßen"
}`

func TestDraftPassesWhenEverySentenceIsSourced(t *testing.T) {
	src := sources(t)
	d, _, err := Write(context.Background(), fake(t, goodDraft),
		Vacancy{Title: "Backend Engineer", Company: "Testfirma", Language: "de"},
		passing(), nil, src, "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := []string{"fact.latency", "go"}
	if strings.Join(d.FactsUsed, ",") != strings.Join(want, ",") {
		t.Errorf("facts_used = %v, want %v", d.FactsUsed, want)
	}

	text := d.Text()
	for _, fragment := range []string{"Sehr geehrte Frau Beispiel", "Lambda", "Mit freundlichen Grüßen"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("the rendered letter is missing %q:\n%s", fragment, text)
		}
	}
}

// The lock is only worth anything if it refuses. Each of these is a way a
// letter can assert something it cannot support.
func TestFactLockRefuses(t *testing.T) {
	src := sources(t)

	cases := map[string]string{
		"a sentence citing nothing": `{"salutation":"Hallo,","closing":"Grüße","paragraphs":[
			{"id":"opening","sentences":[{"text":"Ich bin hochmotiviert.","sources":[]}]}]}`,
		"a sentence citing an id that does not exist": `{"salutation":"Hallo,","closing":"Grüße","paragraphs":[
			{"id":"opening","sentences":[{"text":"Ich kenne Rust.","sources":["rust"]}]}]}`,
		"an empty sentence": `{"salutation":"Hallo,","closing":"Grüße","paragraphs":[
			{"id":"opening","sentences":[{"text":"   ","sources":["go"]}]}]}`,
		"a letter citing nothing at all": `{"salutation":"Hallo,","closing":"Grüße","paragraphs":[]}`,
	}

	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := Write(context.Background(), fake(t, reply),
				Vacancy{Title: "x", Company: "y"}, passing(), nil, src, "")
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, ErrUnsourced) {
				t.Errorf("got %v, want ErrUnsourced", err)
			}
		})
	}
}

// The error has to name the sentence, or a rejected draft cannot be fixed.
func TestRefusalNamesTheSentence(t *testing.T) {
	_, _, err := Write(context.Background(),
		fake(t, `{"salutation":"Hallo,","closing":"Grüße","paragraphs":[
			{"id":"opening","sentences":[{"text":"Ich bin ein Teamplayer.","sources":[]}]}]}`),
		Vacancy{Title: "x", Company: "y"}, passing(), nil, sources(t), "")
	if err == nil || !strings.Contains(err.Error(), "Teamplayer") {
		t.Errorf("got %v, want the offending sentence quoted", err)
	}
}

// The enum is the first layer: an id that does not exist is not something
// the model can return, rather than something rejected afterwards.
func TestSchemaEnumeratesOnlyRealIDs(t *testing.T) {
	src := sources(t)
	s := Schema(src)

	paras := s["properties"].(map[string]any)["paragraphs"].(map[string]any)
	sent := paras["items"].(map[string]any)["properties"].(map[string]any)["sentences"].(map[string]any)
	sources := sent["items"].(map[string]any)["properties"].(map[string]any)["sources"].(map[string]any)
	ids := sources["items"].(map[string]any)["enum"].([]string)

	if len(ids) != len(src.Inventory.Entries)+len(src.Profile.Facts) {
		t.Fatalf("enum has %d ids: %v", len(ids), ids)
	}
	for _, id := range ids {
		if _, ok := src.Resolve(id); !ok {
			t.Errorf("enum offers %q, which resolves to nothing", id)
		}
	}
	// At least one source per sentence, enforced by the schema as well as
	// by the check.
	if sources["minItems"] != 1 {
		t.Error("the schema permits a sentence with no sources")
	}
}

// SPEC.md §13: a letter is written only above the threshold and only
// where no must-have is missing.
func TestEligibility(t *testing.T) {
	if err := Eligible(passing()); err != nil {
		t.Errorf("a passing vacancy was refused: %v", err)
	}

	below := passing()
	below.Score = 79.9
	if err := Eligible(below); !errors.Is(err, ErrBelowThreshold) {
		t.Errorf("got %v, want ErrBelowThreshold", err)
	}

	// A must-gap blocks regardless of the number. The cap keeps such a
	// score under 80 today, but a letter whose honesty depends on a cap
	// depends on the cap never changing.
	gap := passing()
	gap.Score = 95
	gap.MustGaps = []string{"Kafka"}
	err := Eligible(gap)
	if !errors.Is(err, ErrMustGap) {
		t.Errorf("got %v, want ErrMustGap", err)
	}
	if !strings.Contains(err.Error(), "Kafka") {
		t.Errorf("the error does not name the gap: %v", err)
	}
}

func TestIneligibleVacancyNeverReachesTheModel(t *testing.T) {
	f := fake(t, goodDraft)
	below := passing()
	below.Score = 40

	if _, _, err := Write(context.Background(), f, Vacancy{}, below, nil, sources(t), ""); err == nil {
		t.Fatal("a letter was drafted below the threshold")
	}
	if len(f.Calls) != 0 {
		t.Errorf("the model was called %d times for a vacancy that cannot have a letter", len(f.Calls))
	}
}

// An inventory entry with no evidence supports no claim, so offering it
// as a source would invite a sentence that cites an empty string.
func TestEntriesWithoutEvidenceAreNotOffered(t *testing.T) {
	src := sources(t)
	prompt := systemPrompt(src, "")

	if strings.Contains(prompt, "- kafka:") {
		t.Error("an entry with no evidence is offered as a source")
	}
	if !strings.Contains(prompt, "- go: Serverless functions on Lambda") {
		t.Error("an entry with evidence is missing from the sources")
	}
	// A fact that must not be quoted says so.
	if !strings.Contains(prompt, "fact.role") {
		t.Error("profile facts are missing from the sources")
	}
}

// A level the sources record as led rather than written must not become
// hands-on experience in a letter.
func TestManagedEvidenceIsOfferedAsManaged(t *testing.T) {
	prompt := systemPrompt(sources(t), "")
	if !strings.Contains(prompt, "Led the department; did not write the code") {
		t.Error("the managed entry's evidence is not shown to the model")
	}
	if !strings.Contains(prompt, "described as leading the work") {
		t.Error("the instruction about overstating a level is missing")
	}
}

// Uncovered requirements are withheld rather than listed, so the letter
// has nothing to apologise for (SPEC.md §13.4).
func TestUncoveredRequirementsAreNotShown(t *testing.T) {
	reqs := []scoring.Requirement{
		{Text: "Erfahrung mit Go", Kind: scoring.Must, IDs: []string{"go"}, Coverage: 1},
		{Text: "Erfahrung mit Kafka", Kind: scoring.Nice, Coverage: 0},
	}
	prompt := vacancyPrompt(Vacancy{Title: "x", Company: "y"}, reqs)

	if !strings.Contains(prompt, "Erfahrung mit Go") {
		t.Error("a covered requirement is missing")
	}
	if strings.Contains(prompt, "Kafka") {
		t.Error("an uncovered requirement was shown to the model")
	}
}

// SPEC.md §13.2 gives three salutation cases, and the one that matters is
// the middle: a name without a stated gender is addressed without
// guessing one.
func TestSalutationRules(t *testing.T) {
	cases := map[string]struct {
		v    Vacancy
		want string
	}{
		"no contact":    {Vacancy{}, "company team"},
		"female":        {Vacancy{ContactLastName: "Weber", ContactGender: "f"}, "feminine"},
		"male":          {Vacancy{ContactLastName: "Weber", ContactGender: "m"}, "masculine"},
		"gender unsaid": {Vacancy{ContactFirstName: "Kim", ContactLastName: "Weber"}, "do not guess"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := salutationRule(tc.v); !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// The advertisement goes last so the cached prefix survives between
// letters written against one profile.
func TestSystemPromptIsStableAcrossVacancies(t *testing.T) {
	src := sources(t)
	f := fake(t, goodDraft)

	for _, company := range []string{"First GmbH", "Second AG"} {
		if _, _, err := Write(context.Background(), f,
			Vacancy{Title: "Backend", Company: company, AdText: "ad for " + company},
			passing(), nil, src, "style rules here"); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if f.Calls[0].System != f.Calls[1].System {
		t.Error("the system prompt differed between vacancies")
	}
	if !strings.Contains(f.Calls[0].System, "style rules here") {
		t.Error("the style file did not reach the prompt")
	}
	if f.Calls[0].Prompt == f.Calls[1].Prompt {
		t.Error("the vacancy did not reach the user prompt")
	}
}

// The fixed lines are rendered from the style file and written into the
// draft, not asked of the model. A model that returns a near miss - a
// comma dropped, a surname inflected - must not reach the reader, because
// the greeting is the first thing read.
func TestFixedLinesReplaceWhatTheModelReturned(t *testing.T) {
	f := fake(t, goodDraft)
	v := Vacancy{
		Title: "Backend", Company: "Testfirma", Language: "de",
		Salutation: "Guten Tag Kim Weber,",
		Closing:    "Mit freundlichen Grüßen",
	}

	d, _, err := Write(context.Background(), f, v, passing(), nil, sources(t), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if d.Salutation != v.Salutation {
		t.Errorf("salutation = %q, want the rendered one", d.Salutation)
	}
	if !strings.HasPrefix(d.Text(), v.Salutation) {
		t.Error("the rendered letter does not open with the rendered salutation")
	}
	// The model is told which line to use as well, so the body is written
	// to agree with it.
	if !strings.Contains(f.Calls[0].Prompt, "Guten Tag Kim Weber,") {
		t.Error("the salutation did not reach the prompt")
	}
}

// Without them the model is given the rule instead, which is what a
// caller with no style file gets.
func TestWithoutFixedLinesTheModelIsGivenTheRule(t *testing.T) {
	f := fake(t, goodDraft)
	v := Vacancy{Title: "Backend", Company: "Testfirma", ContactFirstName: "Kim", ContactLastName: "Weber"}

	d, _, err := Write(context.Background(), f, v, passing(), nil, sources(t), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if d.Salutation != "Sehr geehrte Frau Beispiel," {
		t.Errorf("the model's own salutation was replaced with %q", d.Salutation)
	}
	if !strings.Contains(f.Calls[0].Prompt, "do not guess") {
		t.Error("the rule did not reach the prompt")
	}
}
