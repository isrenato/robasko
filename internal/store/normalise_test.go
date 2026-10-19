package store

import "testing"

var n = NewNormaliser(nil)

// Two boards writing the same employer differently must reach one company,
// or every source adds its own duplicate of the same firm.
func TestCompanyNamesNormaliseToTheSameIdentity(t *testing.T) {
	groups := [][]string{
		{"Nordwind Logistik GmbH", "Nordwind Logistik", "nordwind logistik gmbh", "  Nordwind   Logistik  GmbH "},
		{"Kastanienhof Verlag AG", "Kastanienhof Verlag"},
		{"Weidmann GmbH & Co. KG", "Weidmann GmbH & Co KG", "Weidmann"},
		{"Grün & Partner", "Gruen und Partner", "GRÜN & PARTNER"},
	}
	for _, g := range groups {
		want := n.Company(g[0])
		if want == "" {
			t.Errorf("%q normalised to nothing", g[0])
		}
		for _, variant := range g[1:] {
			if got := n.Company(variant); got != want {
				t.Errorf("%q -> %q, but %q -> %q", g[0], want, variant, got)
			}
		}
	}
}

func TestDifferentCompaniesStayDifferent(t *testing.T) {
	if n.Company("Nordwind Logistik GmbH") == n.Company("Nordwind Technik GmbH") {
		t.Error("two different employers collapsed into one")
	}
}

// A legal form on its own is not an identity; stripping it must not leave
// an empty string that every such company would then share.
func TestStrippingDoesNotEmptyARealName(t *testing.T) {
	if got := n.Company("AG Solutions GmbH"); got != "ag solutions" {
		t.Errorf("got %q, want %q", got, "ag solutions")
	}
}

func TestFingerprintIgnoresSpellingDifferences(t *testing.T) {
	a := n.Fingerprint("Senior Backend Engineer (m/w/d)", "Nordwind Logistik GmbH", "Berlin")
	b := n.Fingerprint("senior backend engineer m w d", "Nordwind Logistik", "berlin")
	if a != b {
		t.Errorf("the same post fingerprinted differently:\n  %s\n  %s", a, b)
	}
}

func TestFingerprintSeparatesDifferentPosts(t *testing.T) {
	base := n.Fingerprint("Backend Engineer", "Nordwind Logistik", "Berlin")
	for name, other := range map[string]string{
		"title":   n.Fingerprint("Frontend Engineer", "Nordwind Logistik", "Berlin"),
		"company": n.Fingerprint("Backend Engineer", "Kastanienhof Verlag", "Berlin"),
		"city":    n.Fingerprint("Backend Engineer", "Nordwind Logistik", "Hamburg"),
	} {
		if other == base {
			t.Errorf("a different %s produced the same fingerprint", name)
		}
	}
}

// The fields are joined with a separator that cannot occur in them, so
// moving text across a boundary changes the hash.
func TestFingerprintFieldsDoNotRunTogether(t *testing.T) {
	a := n.Fingerprint("Backend", "Engineer Nordwind", "Berlin")
	b := n.Fingerprint("Backend Engineer", "Nordwind", "Berlin")
	if a == b {
		t.Error("field boundaries are not respected")
	}
}

func TestDetectsAdvertisementLanguage(t *testing.T) {
	cases := map[string]string{
		"Wir suchen eine Entwicklerin mit Erfahrung und Kenntnissen für unser Team": "de",
		"We are looking for a developer with experience and skills for our team":    "en",
		"PHP MySQL Docker": "",
	}
	for text, want := range cases {
		if got := n.DetectLanguage(text); got != want {
			t.Errorf("%q -> %q, want %q", text, got, want)
		}
	}
}
