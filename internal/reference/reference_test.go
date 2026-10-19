package reference

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestLongestTermsComeFirst(t *testing.T) {
	// "gmbh co kg" must be tried before "kg", or the shorter form claims
	// part of the longer one and leaves "weidmann gmbh co" behind.
	list := Default().List(LegalForm)
	for i := 1; i < len(list); i++ {
		if len(list[i-1]) < len(list[i]) {
			t.Fatalf("%q precedes the longer %q", list[i-1], list[i])
		}
	}
}

func TestDuplicatesAreDropped(t *testing.T) {
	terms := New(map[Kind][]string{LegalForm: {"gmbh", "gmbh", "", "ag"}})
	if got := len(terms.List(LegalForm)); got != 2 {
		t.Errorf("got %d terms, want 2: %v", got, terms.List(LegalForm))
	}
}

// The version must depend on what the fingerprint depends on, and on
// nothing else: a new softener does not invalidate a stored fingerprint,
// a new legal form does.
func TestFingerprintVersionTracksOnlyFingerprintInputs(t *testing.T) {
	base := New(map[Kind][]string{
		LegalForm: {"gmbh"}, Connector: {"und"}, Softener: {"optional"},
	})
	sameFingerprint := New(map[Kind][]string{
		LegalForm: {"gmbh"}, Connector: {"und"}, Softener: {"optional", "bonus"},
	})
	changed := New(map[Kind][]string{
		LegalForm: {"gmbh", "ag"}, Connector: {"und"}, Softener: {"optional"},
	})

	if base.FingerprintVersion() != sameFingerprint.FingerprintVersion() {
		t.Error("a new softener changed the fingerprint version")
	}
	if base.FingerprintVersion() == changed.FingerprintVersion() {
		t.Error("a new legal form did not change the fingerprint version")
	}
}

func TestFingerprintVersionIgnoresInputOrder(t *testing.T) {
	a := New(map[Kind][]string{LegalForm: {"gmbh", "ag"}, Connector: {"und"}})
	b := New(map[Kind][]string{LegalForm: {"ag", "gmbh"}, Connector: {"und"}})
	if a.FingerprintVersion() != b.FingerprintVersion() {
		t.Error("the version depends on the order terms were listed in")
	}
}

var seedRe = regexp.MustCompile(`\('([a-z_]+)',\s*'((?:[^']|'')*)'\)`)

// The migration seeds the table and Default() stands in without a database.
// If they drift, behaviour depends on whether a database happens to be
// attached, which is the worst kind of difference to debug.
func TestMigrationSeedMatchesTheCompiledDefault(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0002_reference_terms.sql")
	if err != nil {
		t.Fatalf("reading the migration: %v", err)
	}

	seeded := map[Kind]map[string]bool{}
	for _, m := range seedRe.FindAllStringSubmatch(string(raw), -1) {
		kind := Kind(m[1])
		if seeded[kind] == nil {
			seeded[kind] = map[string]bool{}
		}
		seeded[kind][strings.ReplaceAll(m[2], "''", "'")] = true
	}
	if len(seeded) == 0 {
		t.Fatal("no seed rows found; has the migration been renamed?")
	}

	def := Default()
	for _, kind := range []Kind{LegalForm, Connector, Softener, LangMarkerDE, LangMarkerEN} {
		for _, v := range def.List(kind) {
			if !seeded[kind][v] {
				t.Errorf("%s %q is compiled in but not seeded", kind, v)
			}
		}
		for v := range seeded[kind] {
			if !def.Has(kind, v) {
				t.Errorf("%s %q is seeded but not compiled in", kind, v)
			}
		}
	}
}
