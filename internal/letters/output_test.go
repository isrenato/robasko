package letters

import (
	"strings"
	"testing"
)

// SPEC.md §13.5 names the file after the company and the candidate, in
// that order. The name has to survive a filesystem, so the umlauts go
// through transliteration rather than being dropped.
func TestFileName(t *testing.T) {
	cases := map[string]struct {
		company, sender, want string
	}{
		"the ordinary case": {
			"Nordwind Logistik GmbH", "Alex Beispiel",
			"cover-letter_NordwindLogistikGmbH_Beispiel_Alex.txt"},
		"umlauts are spelled out, not dropped": {
			"Grünbau & Söhne AG", "Jörg Müller",
			"cover-letter_GruenbauSoehneAG_Mueller_Joerg.txt"},
		"a middle name joins the first": {
			"Testfirma", "Anna Maria Weber",
			"cover-letter_Testfirma_Weber_AnnaMaria.txt"},
		"one name is a surname": {
			"Testfirma", "Prince", "cover-letter_Testfirma_Prince.txt"},
		"no sender configured": {
			"Testfirma", "", "cover-letter_Testfirma.txt"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FileName(tc.company, tc.sender); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A company name made entirely of punctuation must not produce a file
// called "cover-letter__.txt" or, worse, a path separator.
func TestFileNameDropsEverythingUnsafe(t *testing.T) {
	got := FileName("../../etc", "a/b")
	if strings.ContainsAny(got, `/\`) {
		t.Errorf("%q carries a path separator", got)
	}
	if got != "cover-letter_etc_ab.txt" {
		t.Errorf("got %q", got)
	}
}

func draft() *Draft {
	return &Draft{
		Salutation: "Sehr geehrte Frau Weber,",
		Closing:    "Mit freundlichen Grüßen",
		Paragraphs: []Paragraph{
			{ID: "opening", Sentences: []Sentence{
				{Text: "Sie suchen jemanden für Go.", Sources: []string{"go"}},
			}},
			{ID: "evidence", Sentences: []Sentence{
				{Text: "Ich schreibe Go im Produktivbetrieb.", Sources: []string{"go"}},
				{Text: "Die Latenz fiel von Stunden auf Sekunden.", Sources: []string{"fact.latency"}},
			}},
			{ID: "fit", Sentences: []Sentence{
				{Text: "Ihre Arbeitsweise passt zu meiner.", Sources: []string{"fact.role"}},
			}},
		},
	}
}

// weights are the example style file's: evidence outranks opening, which
// outranks fit.
func weights(id string) int {
	return map[string]int{"opening": 3, "evidence": 5, "fit": 2}[id]
}

// The lowest-weighted paragraph loses material first, and the paragraph
// carrying the evidence is the last thing touched.
func TestShortenCutsTheLowestWeightFirst(t *testing.T) {
	d := draft()

	cut, ok := Shorten(d, weights)
	if !ok {
		t.Fatal("nothing was cut")
	}
	if !strings.Contains(cut, "fit") {
		t.Errorf("cut %q, want the fit paragraph", cut)
	}
	if len(d.Paragraphs) != 2 {
		t.Fatalf("%d paragraphs remain, want 2", len(d.Paragraphs))
	}
	for _, p := range d.Paragraphs {
		if p.ID == "fit" {
			t.Error("the fit paragraph survived")
		}
	}
}

// A paragraph with more than one sentence loses its last sentence rather
// than the whole paragraph: the first sentence makes the point.
func TestShortenTrimsBeforeItRemoves(t *testing.T) {
	d := draft()
	d.Paragraphs[2].Sentences = append(d.Paragraphs[2].Sentences,
		Sentence{Text: "Und das schon lange.", Sources: []string{"fact.role"}})

	cut, ok := Shorten(d, weights)
	if !ok {
		t.Fatal("nothing was cut")
	}
	if !strings.Contains(cut, "sentence") {
		t.Errorf("cut %q, want a sentence", cut)
	}
	if len(d.Paragraphs) != 3 {
		t.Errorf("a paragraph was removed when a sentence would have done")
	}
	if n := len(d.Paragraphs[2].Sentences); n != 1 {
		t.Errorf("fit has %d sentences, want 1", n)
	}
}

// Two one-sentence paragraphs is the floor. Cutting past it produces
// something that is not a letter either, so it refuses and the caller
// marks the draft for a human.
func TestShortenRefusesToCutPastALetter(t *testing.T) {
	d := &Draft{Paragraphs: []Paragraph{
		{ID: "opening", Sentences: []Sentence{{Text: "a", Sources: []string{"go"}}}},
		{ID: "evidence", Sentences: []Sentence{{Text: "b", Sources: []string{"go"}}}},
	}}
	if _, ok := Shorten(d, weights); ok {
		t.Error("cut a two-sentence letter further")
	}
	if len(d.Paragraphs) != 2 {
		t.Error("the refusal still changed the draft")
	}
}

// facts_used is stored beside the letter, so it has to describe the
// letter as sent and not an earlier, longer version of it.
func TestShortenUpdatesTheFactsUsed(t *testing.T) {
	d := draft()
	d.FactsUsed = []string{"fact.latency", "fact.role", "go"}

	if _, ok := Shorten(d, weights); !ok {
		t.Fatal("nothing was cut")
	}
	got := strings.Join(d.FactsUsed, ",")
	if got != "fact.latency,go" {
		t.Errorf("facts_used = %q, want the ids the shortened letter still cites", got)
	}
}

// With no weights at all - a style file with no structure section - the
// tie goes to the later paragraph, which keeps the opening intact.
func TestShortenWithoutWeightsKeepsTheOpening(t *testing.T) {
	d := draft()
	cut, ok := Shorten(d, func(string) int { return 0 })
	if !ok {
		t.Fatal("nothing was cut")
	}
	if !strings.Contains(cut, "fit") {
		t.Errorf("cut %q, want the last paragraph", cut)
	}
}

func TestWordsCountsTheBodyOnly(t *testing.T) {
	// 5 + 5 + 7 + 5; the salutation and the closing are not body.
	if got := draft().Words(); got != 22 {
		t.Errorf("Words() = %d", got)
	}
}
