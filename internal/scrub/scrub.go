// Package scrub replaces personal data with stable pseudonyms.
//
// Recorded language-model fixtures are committed so that tests run without
// network access, which makes them the one path where personal data would
// otherwise reach version control on purpose. Scrubbing happens when a
// fixture is recorded rather than when it is committed: the unscrubbed form
// is never written, so there is nothing for a later check to miss.
//
// Pseudonyms are derived deterministically from the real value, so the same
// person named in an advertisement and in the letter answering it becomes the
// same pseudonym in both. The key lives in the private data root, so the
// mapping cannot be reversed by a reader of the public repository.
package scrub

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Category selects the pseudonym table a target is drawn from.
type Category int

const (
	Person Category = iota
	Company
	Address
	Email
	Phone
)

// Target is a literal value to replace. Names, companies and addresses cannot
// be recognised by shape, so the recorder supplies them from the records it
// is scrubbing; email addresses and telephone numbers are additionally found
// by pattern.
type Target struct {
	Value    string
	Category Category
}

// KeyFileName is the key's name within the private data root.
const KeyFileName = "fixture-scrub.key"

// contentKeys are the JSON object keys whose string values carry message
// content. Everything else - tool names, schema keys, token counts, model
// identifiers - is left exactly as recorded.
var contentKeys = map[string]bool{
	"text":    true,
	"system":  true,
	"content": true,
}

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// A telephone number is a run of at least seven digits, freely broken by
	// spaces, dashes, slashes, dots or parentheses, optionally preceded by a
	// country prefix.
	phoneRe = regexp.MustCompile(`(?:\+|00)?[0-9][0-9().\-/ ]{5,}[0-9]`)
)

// Matcher recognises target values in text, ignoring case, diacritics and -
// within a run of digits - the separators between them.
//
// It is shared with the pre-commit check, so a value the scrubber would have
// replaced is exactly a value the check rejects. Two implementations of this
// matching would drift, and the gap between them would be the leak.
type Matcher struct {
	patterns []compiledTarget
}

type compiledTarget struct {
	re       *regexp.Regexp
	value    string
	category Category
	digits   string // phone targets only: the value reduced to digits
}

// Match reports one target found in a piece of text.
type Match struct {
	Value    string   // the configured target
	Category Category // the table it belongs to
	Found    string   // the spelling that actually appeared
}

// NewMatcher compiles the supplied targets.
func NewMatcher(targets []Target) (*Matcher, error) {
	m := &Matcher{}

	// Longest first, so "Nordwind Logistik GmbH" is consumed before the
	// shorter "Nordwind" can claim part of it.
	sorted := append([]Target(nil), targets...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(sorted[i].Value) > len(sorted[j].Value)
	})

	for _, t := range sorted {
		if strings.TrimSpace(t.Value) == "" {
			continue
		}
		// A telephone number is compared digit by digit rather than as
		// text: the same number is written with spaces, dashes, slashes or
		// parentheses, and with +49, 0049 or a national 0 in front, and all
		// of those are one number.
		if t.Category == Phone {
			m.patterns = append(m.patterns, compiledTarget{
				value: t.Value, category: Phone, digits: digitsOnly(t.Value),
			})
			continue
		}
		re, err := literalPattern(t.Value)
		if err != nil {
			return nil, fmt.Errorf("scrub: compiling target %q: %w", t.Value, err)
		}
		m.patterns = append(m.patterns, compiledTarget{re: re, value: t.Value, category: t.Category})
	}
	return m, nil
}

// Find returns every target occurring in text, at most once per target.
func (m *Matcher) Find(text string) []Match {
	var out []Match
	for _, p := range m.patterns {
		if p.category == Phone {
			if found, ok := findPhone(text, p.digits); ok {
				out = append(out, Match{Value: p.value, Category: Phone, Found: found})
			}
			continue
		}
		if found := p.re.FindString(text); found != "" {
			out = append(out, Match{Value: p.value, Category: p.category, Found: found})
		}
	}
	return out
}

// minPhoneDigits is the shortest overlap treated as the same number. Below
// this, two unrelated numbers routinely share a tail.
const minPhoneDigits = 7

// findPhone looks for a telephone number in text whose digits are the same
// number as want, allowing for separators and for the international and
// national prefixes being written differently.
func findPhone(text, want string) (string, bool) {
	for _, candidate := range phoneRe.FindAllString(text, -1) {
		if samePhone(digitsOnly(candidate), want) {
			return strings.TrimSpace(candidate), true
		}
	}
	return "", false
}

// samePhone reports whether two digit strings denote the same number. One is
// allowed to be a suffix of the other, which is how a national spelling
// (0561 ...) relates to an international one (+49 561 ...).
func samePhone(a, b string) bool {
	for _, x := range phoneForms(a) {
		for _, y := range phoneForms(b) {
			if len(x) < minPhoneDigits || len(y) < minPhoneDigits {
				continue
			}
			if strings.HasSuffix(x, y) || strings.HasSuffix(y, x) {
				return true
			}
		}
	}
	return false
}

// phoneForms returns the digit string with its international and national
// prefixes progressively removed.
func phoneForms(d string) []string {
	forms := []string{d}
	if trimmed, ok := strings.CutPrefix(d, "00"); ok {
		forms = append(forms, trimmed)
		d = trimmed
	}
	if trimmed, ok := strings.CutPrefix(d, "0"); ok {
		forms = append(forms, trimmed)
	}
	return forms
}

// Scrubber replaces personal data in recorded fixtures.
type Scrubber struct {
	key     []byte
	matcher *Matcher
}

// New returns a Scrubber keyed by key, recognising the supplied targets in
// addition to the email and telephone patterns.
func New(key []byte, targets []Target) (*Scrubber, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("scrub: empty key")
	}
	m, err := NewMatcher(targets)
	if err != nil {
		return nil, err
	}
	return &Scrubber{key: append([]byte(nil), key...), matcher: m}, nil
}

// LoadOrCreateKey reads the scrubbing key from the private data root,
// generating one on first use. The key file is owner-readable only.
func LoadOrCreateKey(dataRoot string) ([]byte, error) {
	path := filepath.Join(dataRoot, KeyFileName)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, decodeErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decodeErr != nil {
			return nil, fmt.Errorf("scrub: key at %s is malformed: %w", path, decodeErr)
		}
		if len(key) == 0 {
			return nil, fmt.Errorf("scrub: key at %s is empty", path)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("scrub: reading key: %w", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("scrub: generating key: %w", err)
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return nil, fmt.Errorf("scrub: creating data root: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("scrub: writing key: %w", err)
	}
	return key, nil
}

// Text replaces every recognised value in s with its pseudonym.
func (s *Scrubber) Text(in string) string {
	out := emailRe.ReplaceAllStringFunc(in, func(m string) string {
		return s.pseudonym(m, Email)
	})
	out = phoneRe.ReplaceAllStringFunc(out, func(m string) string {
		if countDigits(m) < 7 {
			return m
		}
		// Preserve any leading and trailing whitespace the loose pattern
		// swept up, so surrounding prose keeps its spacing.
		lead := m[:len(m)-len(strings.TrimLeft(m, " "))]
		trail := m[len(strings.TrimRight(m, " ")):]
		return lead + s.pseudonym(strings.TrimSpace(m), Phone) + trail
	})
	for _, p := range s.matcher.patterns {
		// Phone targets carry no regexp: every telephone number, listed or
		// not, was already replaced by the pattern pass above.
		if p.category == Phone {
			continue
		}
		out = p.re.ReplaceAllString(out, s.pseudonym(p.value, p.category))
	}
	return out
}

// JSON scrubs the string values of content-bearing keys in a recorded
// fixture, leaving every other byte - keys, structural punctuation,
// whitespace, numbers, tool names - exactly as it was.
func (s *Scrubber) JSON(in []byte) ([]byte, error) {
	var out strings.Builder
	out.Grow(len(in))

	// keyStack holds the most recent object key at each nesting level. An
	// array inherits its parent's key, so the strings inside
	// "content": ["..."] are scrubbed too.
	keyStack := []string{""}

	i := 0
	for i < len(in) {
		c := in[i]
		switch c {
		case '{':
			keyStack = append(keyStack, "")
			out.WriteByte(c)
			i++
		case '[':
			keyStack = append(keyStack, keyStack[len(keyStack)-1])
			out.WriteByte(c)
			i++
		case '}', ']':
			if len(keyStack) > 1 {
				keyStack = keyStack[:len(keyStack)-1]
			}
			out.WriteByte(c)
			i++
		case '"':
			end, err := scanString(in, i)
			if err != nil {
				return nil, err
			}
			literal := in[i:end]
			if isKey(in, end) {
				var k string
				if err := json.Unmarshal(literal, &k); err != nil {
					return nil, fmt.Errorf("scrub: decoding key: %w", err)
				}
				keyStack[len(keyStack)-1] = k
				out.Write(literal)
			} else if contentKeys[keyStack[len(keyStack)-1]] {
				var v string
				if err := json.Unmarshal(literal, &v); err != nil {
					return nil, fmt.Errorf("scrub: decoding value: %w", err)
				}
				encoded, err := json.Marshal(s.Text(v))
				if err != nil {
					return nil, fmt.Errorf("scrub: encoding value: %w", err)
				}
				out.Write(encoded)
			} else {
				out.Write(literal)
			}
			i = end
		default:
			out.WriteByte(c)
			i++
		}
	}
	return []byte(out.String()), nil
}

// pseudonym derives a stable replacement for value within its category.
func (s *Scrubber) pseudonym(value string, c Category) string {
	n := s.index(value, c)
	switch c {
	case Person:
		return firstNames[n%uint64(len(firstNames))] + " " +
			lastNames[(n/uint64(len(firstNames)))%uint64(len(lastNames))]
	case Company:
		return companyNames[n%uint64(len(companyNames))]
	case Address:
		return fmt.Sprintf("%s %d", streetNames[n%uint64(len(streetNames))], 1+n%89)
	case Email:
		return fmt.Sprintf("%s.%s@example.invalid",
			strings.ToLower(firstNames[n%uint64(len(firstNames))]),
			strings.ToLower(lastNames[(n/uint64(len(firstNames)))%uint64(len(lastNames))]))
	case Phone:
		return fmt.Sprintf("+49 30 %06d", n%1000000)
	default:
		return "[redacted]"
	}
}

// index is an HMAC of the normalised value, so the same real value always
// selects the same pseudonym and the mapping cannot be reversed without the
// key.
func (s *Scrubber) index(value string, c Category) uint64 {
	mac := hmac.New(sha256.New, s.key)
	fmt.Fprintf(mac, "%d:", c)
	mac.Write([]byte(normalise(value, c)))
	return binary.BigEndian.Uint64(mac.Sum(nil)[:8])
}

// normalise reduces a value to the form two spellings of it share, so that
// "Müller" and "mueller" - or a telephone number written with and without
// separators - resolve to one pseudonym.
func normalise(value string, c Category) string {
	if c == Phone {
		return digitsOnly(value)
	}
	return strings.Join(strings.Fields(strings.ToLower(fold(value))), " ")
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func countDigits(s string) int { return len(digitsOnly(s)) }

// diacritics maps accented letters onto the plain letter they fold to.
var diacritics = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'ā': "a",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ē': "e",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ī': "i",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'ō': "o",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ū': "u",
	'ç': "c", 'ñ': "n", 'ý': "y", 'ÿ': "y", 'š': "s", 'ž': "z", 'ß': "ss",
}

// fold removes diacritics so that matching ignores them.
func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if rep, ok := diacritics[r]; ok {
			b.WriteString(rep)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// expansions maps a plain letter back to the character class matching it and
// every accented form that folds to it.
var expansions = func() map[rune]string {
	m := map[rune][]rune{}
	for accented, plain := range diacritics {
		if len([]rune(plain)) == 1 {
			p := []rune(plain)[0]
			m[p] = append(m[p], accented)
		}
	}
	out := map[rune]string{}
	for plain, accents := range m {
		sort.Slice(accents, func(i, j int) bool { return accents[i] < accents[j] })
		var cls strings.Builder
		cls.WriteByte('[')
		cls.WriteRune(plain)
		cls.WriteRune(plain - 32) // the upper-case form
		for _, a := range accents {
			cls.WriteRune(a)
			cls.WriteString(strings.ToUpper(string(a)))
		}
		cls.WriteByte(']')

		// German transliterates its umlauts as digraphs when the accented
		// character is unavailable, and advertisements use both spellings
		// interchangeably: Mueller and Müller are one person.
		if plain == 'a' || plain == 'o' || plain == 'u' {
			out[plain] = "(?:" + cls.String() + "|" + string(plain) + "e)"
			continue
		}
		out[plain] = cls.String()
	}
	return out
}()

// literalPattern compiles a target into a regexp matching it regardless of
// case, diacritics, or - for a run of digits - the separators between them.
func literalPattern(value string) (*regexp.Regexp, error) {
	folded := fold(value)
	var b strings.Builder
	runes := []rune(folded)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == 's' && i+1 < len(runes) && runes[i+1] == 's':
			// "ss" also matches the eszett it folds from.
			b.WriteString("(?:[sS][sS]|ß)")
			i++
		case r >= '0' && r <= '9':
			// Digits may be separated arbitrarily in the text.
			b.WriteString(regexp.QuoteMeta(string(r)))
			if i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9' {
				b.WriteString(`[ ().\-/]*`)
			}
		case r == ' ':
			b.WriteString(`\s+`)
		default:
			if cls, ok := expansions[r]; ok {
				b.WriteString(cls)
				continue
			}
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return regexp.Compile(`(?i)` + b.String())
}

// scanString returns the index just past the JSON string literal starting at
// start, honouring backslash escapes.
func scanString(in []byte, start int) (int, error) {
	i := start + 1
	for i < len(in) {
		switch in[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1, nil
		}
		i++
	}
	return 0, fmt.Errorf("scrub: unterminated string at byte %d", start)
}

// isKey reports whether the string literal ending at end is an object key,
// which is to say the next meaningful byte is a colon.
func isKey(in []byte, end int) bool {
	for i := end; i < len(in); i++ {
		switch in[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case ':':
			return true
		default:
			return false
		}
	}
	return false
}

var (
	firstNames = []string{
		"Jordan", "Mika", "Robin", "Alex", "Toni", "Sam", "Kim", "Noa",
		"Luca", "Andrea", "Charlie", "Dominique", "Eli", "Frankie", "Jules", "Rene",
	}
	lastNames = []string{
		"Vasquez", "Lindqvist", "Brenner", "Okafor", "Dubois", "Halvorsen",
		"Moreau", "Ferreira", "Novak", "Keller", "Ibrahim", "Lindgren",
		"Fontaine", "Weber", "Sandoval", "Petrov",
	}
	companyNames = []string{
		"Nordwind Logistik GmbH", "Kastanienhof Verlag AG", "Steinbach Software",
		"Lumenstadt Technik", "Birkenallee Systeme", "Hafenkante Digital",
		"Rosengarten Medien", "Talweg Industrie", "Seeblick Consulting",
		"Altmarkt Datentechnik", "Weidenhof Labs", "Sonnenfeld Werke",
		"Kieferberg Analytik", "Uferstrasse Mobil", "Lindenplatz Software",
		"Morgentau Handel",
	}
	streetNames = []string{
		"Birkenweg", "Lindenstrasse", "Rosenallee", "Hafenstrasse", "Talweg",
		"Seeblickweg", "Altmarkt", "Weidenhof", "Sonnenfeldstrasse", "Kieferberg",
		"Uferstrasse", "Lindenplatz", "Morgentauweg", "Kastanienallee",
		"Steinbachweg", "Nordwindstrasse",
	}
)
