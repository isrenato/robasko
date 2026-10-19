package scrub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testKey = []byte("a-fixed-key-for-deterministic-tests")

func newScrubber(t *testing.T, targets ...Target) *Scrubber {
	t.Helper()
	s, err := New(testKey, targets)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return s
}

func TestReplacesEachCategory(t *testing.T) {
	s := newScrubber(t,
		Target{Value: "Renate Baumgartner", Category: Person},
		Target{Value: "Hochbrueck Industrie GmbH", Category: Company},
		Target{Value: "Ahornweg 14", Category: Address},
	)

	cases := []struct {
		name string
		in   string
	}{
		{"person", "Ansprechpartnerin ist Renate Baumgartner."},
		{"company", "Die Hochbrueck Industrie GmbH sucht Verstaerkung."},
		{"address", "Unser Buero liegt am Ahornweg 14 in Kassel."},
		{"email", "Bewerbungen an r.baumgartner@hochbrueck-industrie.de erbeten."},
		{"phone", "Rueckfragen unter +49 561 7788990."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Text(tc.in)
			if got == tc.in {
				t.Fatalf("nothing was replaced in %q", tc.in)
			}
			for _, secret := range []string{
				"Renate Baumgartner", "Hochbrueck Industrie GmbH", "Ahornweg 14",
				"r.baumgartner@hochbrueck-industrie.de", "7788990",
			} {
				if strings.Contains(got, secret) {
					t.Errorf("output still contains %q: %s", secret, got)
				}
			}
		})
	}
}

func TestPseudonymsAreStableAcrossInstances(t *testing.T) {
	targets := []Target{{Value: "Renate Baumgartner", Category: Person}}
	first := newScrubber(t, targets...)
	second := newScrubber(t, targets...)

	in := "Renate Baumgartner leitet das Team."
	a, b := first.Text(in), second.Text(in)
	if a != b {
		t.Errorf("two instances disagreed:\n  %s\n  %s", a, b)
	}

	// The same person named in two different fixtures must come out the
	// same, otherwise a replayed conversation contradicts itself.
	ad := first.Text("Ansprechpartnerin: Renate Baumgartner")
	letter := second.Text("Sehr geehrte Frau Renate Baumgartner,")
	name := strings.TrimPrefix(strings.TrimSuffix(ad, ""), "Ansprechpartnerin: ")
	if !strings.Contains(letter, name) {
		t.Errorf("pseudonym differed between fixtures: %q vs %q", ad, letter)
	}
}

func TestDifferentKeysProduceDifferentPseudonyms(t *testing.T) {
	targets := []Target{{Value: "Renate Baumgartner", Category: Person}}
	a := newScrubber(t, targets...)
	other, err := New([]byte("a-different-key"), targets)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if a.Text("Renate Baumgartner") == other.Text("Renate Baumgartner") {
		t.Error("pseudonym did not depend on the key")
	}
}

const fixture = `{
  "model": "claude-opus-5-5",
  "max_tokens": 16000,
  "tools": [
    {
      "name": "vacancy.extract_requirements",
      "input_schema": {
        "type": "object",
        "additionalProperties": false,
        "required": ["vacancy_id"]
      }
    }
  ],
  "system": "Extract requirements verbatim.",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "Ansprechpartnerin: Renate Baumgartner, Tel. +49 561 7788990"}
      ]
    }
  ],
  "usage": {"input_tokens": 1234, "output_tokens": 567}
}`

func TestJSONLeavesStructuralFieldsUntouched(t *testing.T) {
	s := newScrubber(t, Target{Value: "Renate Baumgartner", Category: Person})

	out, err := s.JSON([]byte(fixture))
	if err != nil {
		t.Fatalf("JSON returned error: %v", err)
	}
	got := string(out)

	// Structural fields survive byte for byte, including key order,
	// indentation, numbers and the tool name.
	structural := []string{
		`"model": "claude-opus-5-5"`,
		`"max_tokens": 16000`,
		`"name": "vacancy.extract_requirements"`,
		`"type": "object"`,
		`"additionalProperties": false`,
		`"required": ["vacancy_id"]`,
		`"role": "user"`,
		`{"type": "text", "text":`,
		`"usage": {"input_tokens": 1234, "output_tokens": 567}`,
	}
	for _, want := range structural {
		if !strings.Contains(got, want) {
			t.Errorf("structural fragment lost: %s", want)
		}
	}

	if strings.Contains(got, "Renate Baumgartner") {
		t.Error("the contact name survived scrubbing")
	}
	if strings.Contains(got, "7788990") {
		t.Error("the telephone number survived scrubbing")
	}
	if !json.Valid(out) {
		t.Error("output is not valid JSON")
	}
}

// "type": "text" is a schema value that happens to sit under a key named
// "type"; only values under content-bearing keys may be rewritten.
func TestJSONDoesNotScrubKeysOrSchemaValues(t *testing.T) {
	s := newScrubber(t, Target{Value: "text", Category: Person})

	out, err := s.JSON([]byte(`{"type": "text", "text": "hello"}`))
	if err != nil {
		t.Fatalf("JSON returned error: %v", err)
	}
	if !strings.Contains(string(out), `"type": "text"`) {
		t.Errorf("a schema value was rewritten: %s", out)
	}
	if !strings.Contains(string(out), `"text": "`) {
		t.Errorf("the content key itself was rewritten: %s", out)
	}
}

func TestJSONScrubsStringsInsideContentArrays(t *testing.T) {
	s := newScrubber(t, Target{Value: "Renate Baumgartner", Category: Person})

	out, err := s.JSON([]byte(`{"content": ["Renate Baumgartner", "second"]}`))
	if err != nil {
		t.Fatalf("JSON returned error: %v", err)
	}
	if strings.Contains(string(out), "Renate Baumgartner") {
		t.Errorf("array element not scrubbed: %s", out)
	}
}

func TestMatchesPhoneNumbersAcrossGroupings(t *testing.T) {
	s := newScrubber(t)

	variants := []string{
		"+49 561 7788990",
		"+49 (561) 77-88-990",
		"0049 561 778 8990",
		"0561/7788990",
	}
	for _, v := range variants {
		got := s.Text("Tel. " + v)
		if strings.Contains(got, "7788990") || strings.Contains(got, "88-990") {
			t.Errorf("grouping %q was not matched: %s", v, got)
		}
	}
}

func TestMatchesNamesAcrossCaseAndDiacritics(t *testing.T) {
	s := newScrubber(t, Target{Value: "Jürgen Müller", Category: Person})

	for _, v := range []string{"Jürgen Müller", "JÜRGEN MÜLLER", "jurgen muller", "Jurgen Mueller"} {
		got := s.Text("Kontakt: " + v)
		if strings.Contains(strings.ToLower(got), "ller") {
			t.Errorf("spelling %q was not matched: %s", v, got)
		}
	}
}

func TestEszettFoldsToDoubleS(t *testing.T) {
	s := newScrubber(t, Target{Value: "Strassmann", Category: Person})

	got := s.Text("Kontakt: Straßmann")
	if strings.Contains(got, "Straßmann") {
		t.Errorf("eszett spelling was not matched: %s", got)
	}
}

func TestLongerTargetsWinOverShorter(t *testing.T) {
	s := newScrubber(t,
		Target{Value: "Nordwind", Category: Company},
		Target{Value: "Nordwind Logistik GmbH", Category: Company},
	)

	got := s.Text("Nordwind Logistik GmbH sucht.")
	if strings.Contains(got, "Logistik GmbH") {
		t.Errorf("the shorter target consumed part of the longer one: %s", got)
	}
}

func TestLoadOrCreateKeyRoundTrips(t *testing.T) {
	root := t.TempDir()

	first, err := LoadOrCreateKey(root)
	if err != nil {
		t.Fatalf("first LoadOrCreateKey: %v", err)
	}
	if len(first) != 32 {
		t.Errorf("got a %d-byte key, want 32", len(first))
	}

	info, err := os.Stat(filepath.Join(root, KeyFileName))
	if err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode %04o, want 0600", perm)
	}

	second, err := LoadOrCreateKey(root)
	if err != nil {
		t.Fatalf("second LoadOrCreateKey: %v", err)
	}
	if string(first) != string(second) {
		t.Error("key was regenerated instead of reused")
	}
}

func TestRejectsEmptyKey(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Error("expected an empty key to be rejected")
	}
}
