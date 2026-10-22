package style

import (
	"strings"
	"testing"
)

const sample = `
sender:
  name: Alex Beispiel
  lines:
    - Alex Beispiel
    - alex@example.org

languages:
  de:
    address_as: Sie
    subject: "Bewerbung als {title}"
    salutation:
      gendered_female: "Sehr geehrte Frau {last_name},"
      gendered_male: "Sehr geehrter Herr {last_name},"
      ungendered: "Guten Tag {first_name} {last_name},"
      no_contact: "Liebes {company}-Team,"
    closing: "Mit freundlichen Grüßen"
    date_format: "2. January 2006"
  en:
    address_as: you
    subject: "Application for {title}"
    salutation:
      gendered_female: "Dear Ms {last_name},"
      gendered_male: "Dear Mr {last_name},"
      ungendered: "Dear {first_name} {last_name},"
      no_contact: "Dear {company} team,"
    closing: "Kind regards"

length:
  max_pages: 1
  target_words: 240
  max_words: 300

structure:
  - id: opening
    weight: 3
    intent: Name the role.
    sentences: [1, 2]
  - id: evidence
    weight: 5
    intent: The strongest requirements, each answered with something done.
  - id: fit
    weight: 2
    intent: Why this employer.
    optional: true

claims:
  - "Never state a level above what the inventory records."

tired_openings:
  - "hiermit bewerbe ich mich"

voice_sample: >
  I keep sentences short. Then I stop.
`

func load(t *testing.T) *Style {
	t.Helper()
	s, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

// SPEC.md §13.2's four cases. The third is the one that matters: a name
// whose gender the advertisement does not state is addressed without a
// guess, because guessing is wrong about some people every time and it is
// wrong in the first line.
func TestSalutationCases(t *testing.T) {
	s := load(t)

	cases := map[string]struct {
		contact Contact
		want    string
	}{
		"no contact":    {Contact{Company: "Nordwind"}, "Liebes Nordwind-Team,"},
		"female":        {Contact{LastName: "Weber", Gender: "f"}, "Sehr geehrte Frau Weber,"},
		"male":          {Contact{LastName: "Weber", Gender: "m"}, "Sehr geehrter Herr Weber,"},
		"gender unsaid": {Contact{FirstName: "Kim", LastName: "Weber"}, "Guten Tag Kim Weber,"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := s.Salutation("de", tc.contact); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A gender the file has no form for falls back to the ungendered one
// rather than to the other gender.
func TestSalutationFallsBackWithoutGuessing(t *testing.T) {
	s := load(t)
	de := s.Languages["de"]
	de.Salutation.GenderedFemale = ""
	s.Languages["de"] = de

	got := s.Salutation("de", Contact{FirstName: "Kim", LastName: "Weber", Gender: "f"})
	if got != "Guten Tag Kim Weber," {
		t.Errorf("got %q, want the ungendered form", got)
	}
}

// An advertisement in a language the file does not cover still gets a
// closing line, because a letter without one cannot be sent at all.
func TestUnknownLanguageFallsBack(t *testing.T) {
	s := load(t)
	if got := s.Closing("fr"); got != "Kind regards" {
		t.Errorf("got %q, want the English closing", got)
	}
	if got := s.Salutation("fr", Contact{Company: "X"}); got != "Dear X team," {
		t.Errorf("got %q", got)
	}
}

func TestSubjectAndDateFormat(t *testing.T) {
	s := load(t)
	if got := s.Subject("de", "Backend Engineer"); got != "Bewerbung als Backend Engineer" {
		t.Errorf("got %q", got)
	}
	// English has no date_format; the fallback is a real layout, not an
	// empty string that would render as the layout itself.
	if got := s.DateFormat("en"); got != "2 January 2006" {
		t.Errorf("got %q", got)
	}
	if got := s.DateFormat("de"); got != "2. January 2006" {
		t.Errorf("got %q", got)
	}
}

// The weight is what the one-page retry cuts by, so a section the file
// does not describe must not outrank one it does.
func TestWeights(t *testing.T) {
	s := load(t)
	if got := s.Weight("evidence"); got != 5 {
		t.Errorf("evidence weighs %d, want 5", got)
	}
	if got := s.Weight("fit"); got != 2 {
		t.Errorf("fit weighs %d, want 2", got)
	}
	if got := s.Weight("something-the-model-invented"); got != 0 {
		t.Errorf("an undescribed section weighs %d, want 0", got)
	}
}

func TestPromptCarriesTheStructureAndTheLimits(t *testing.T) {
	prompt := load(t).Prompt()

	for _, want := range []string{
		"240", "300",
		"opening (1-2 sentences)", "evidence", "fit", "(optional)",
		"Never state a level above what the inventory records.",
		"hiermit bewerbe ich mich",
		"I keep sentences short.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// The example file ships with instructions where the voice sample goes.
// Feeding those to the model would teach it to write about writing.
func TestPlaceholderVoiceSampleIsWithheld(t *testing.T) {
	s := load(t)
	s.VoiceSample = "Replace this with two or three sentences you actually wrote."

	if strings.Contains(s.Prompt(), "Replace this") {
		t.Error("the placeholder reached the prompt")
	}
	if len(s.Warnings()) == 0 {
		t.Error("a placeholder voice sample produced no warning")
	}
}

func TestParseRefusesAFileThatCannotProduceALetter(t *testing.T) {
	cases := map[string]string{
		"no languages":  "structure: []\n",
		"no closing":    "languages:\n  de:\n    salutation:\n      no_contact: \"Hallo,\"\n",
		"no salutation": "languages:\n  de:\n    closing: \"Grüße\"\n",
		"a duplicate section": "languages:\n  de:\n    closing: \"Grüße\"\n" +
			"    salutation:\n      no_contact: \"Hallo,\"\n" +
			"structure:\n  - id: opening\n    weight: 3\n  - id: opening\n    weight: 1\n",
		"a section with no id": "languages:\n  de:\n    closing: \"Grüße\"\n" +
			"    salutation:\n      no_contact: \"Hallo,\"\n" +
			"structure:\n  - weight: 3\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Error("accepted")
			}
		})
	}
}
