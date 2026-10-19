package store

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/isrenato/robasko/internal/reference"
)

var (
	punctuationRe = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)
	whitespaceRe  = regexp.MustCompile(`\s+`)
)

// diacritics folds the accented characters German and its neighbours use,
// so one spelling of a name matches another. This stays compiled in: it is
// orthography, not vocabulary, and it does not change.
var diacritics = strings.NewReplacer(
	"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
	"á", "a", "à", "a", "â", "a", "å", "a",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o", "ø", "o",
	"ú", "u", "ù", "u", "û", "u",
	"ç", "c", "ñ", "n", "ý", "y",
)

// Normaliser reduces names and text to comparable forms using a loaded
// vocabulary.
type Normaliser struct{ terms *reference.Terms }

// NewNormaliser returns a normaliser over the given vocabulary. A nil
// vocabulary falls back to the one compiled in.
func NewNormaliser(terms *reference.Terms) *Normaliser {
	if terms == nil {
		terms = reference.Default()
	}
	return &Normaliser{terms: terms}
}

// Terms exposes the vocabulary in use, for recording which one a
// fingerprint was computed under.
func (n *Normaliser) Terms() *reference.Terms { return n.terms }

// Company reduces a company name to what identifies it: the legal form and
// the connectors carry no identity, so "Nordwind Logistik GmbH" and
// "Nordwind Logistik" are one employer.
func (n *Normaliser) Company(name string) string {
	s := normaliseBase(name)

	var kept []string
	for _, word := range strings.Fields(s) {
		if !n.terms.Has(reference.Connector, word) {
			kept = append(kept, word)
		}
	}
	s = strings.Join(kept, " ")

	// Repeatedly, because "Foo GmbH & Co. KG" sheds more than one.
	for changed := true; changed; {
		changed = false
		for _, form := range n.terms.List(reference.LegalForm) {
			if trimmed, ok := strings.CutSuffix(s, " "+form); ok {
				s, changed = strings.TrimSpace(trimmed), true
				break
			}
		}
	}
	return s
}

// Text reduces any other field to a comparable form.
func (n *Normaliser) Text(s string) string { return normaliseBase(s) }

func normaliseBase(s string) string {
	s = diacritics.Replace(strings.ToLower(strings.TrimSpace(s)))
	s = punctuationRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " "))
}

// Fingerprint identifies a vacancy independently of which board carried it,
// so the same post arriving from two sources is stored once.
//
// It depends on the vocabulary, which is why every stored row also records
// Terms().FingerprintVersion().
func (n *Normaliser) Fingerprint(title, company, city string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		n.Text(title), n.Company(company), n.Text(city),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// DetectLanguage reports "de" or "en" for an advertisement by counting
// function words, and "" when the text gives no evidence either way, which
// is stored as NULL rather than guessed at.
func (n *Normaliser) DetectLanguage(text string) string {
	s := " " + strings.ToLower(whitespaceRe.ReplaceAllString(text, " ")) + " "
	de, en := 0, 0
	for _, m := range n.terms.List(reference.LangMarkerDE) {
		de += strings.Count(s, m)
	}
	for _, m := range n.terms.List(reference.LangMarkerEN) {
		en += strings.Count(s, m)
	}
	switch {
	case de > en:
		return "de"
	case en > de:
		return "en"
	default:
		return ""
	}
}
