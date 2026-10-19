// Package reference holds the vocabulary that matching depends on: the
// legal forms stripped from a company name, the connectors ignored in one,
// the phrases that soften a requirement into a preference, and the function
// words that tell German prose from English.
//
// It is data, not logic. It grows whenever an unfamiliar spelling turns up,
// and that should not require a rebuild, so the authority is the
// reference_terms table. Default() returns the same vocabulary compiled in,
// for tests and for commands that run without a database.
package reference

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Kind names a list.
type Kind string

const (
	LegalForm    Kind = "legal_form"
	Connector    Kind = "connector"
	Softener     Kind = "softener"
	LangMarkerDE Kind = "lang_marker_de"
	LangMarkerEN Kind = "lang_marker_en"
)

// Terms is one loaded vocabulary.
type Terms struct {
	lists map[Kind][]string
	set   map[Kind]map[string]bool
}

// New builds a vocabulary from term lists.
func New(lists map[Kind][]string) *Terms {
	t := &Terms{
		lists: make(map[Kind][]string, len(lists)),
		set:   make(map[Kind]map[string]bool, len(lists)),
	}
	for kind, values := range lists {
		seen := map[string]bool{}
		var kept []string
		for _, v := range values {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			kept = append(kept, v)
		}
		// Longest first: "gmbh co kg" has to be tried before "kg", or the
		// shorter form claims part of the longer one.
		sort.SliceStable(kept, func(a, b int) bool { return len(kept[a]) > len(kept[b]) })
		t.lists[kind] = kept
		t.set[kind] = seen
	}
	return t
}

// List returns the terms of a kind, longest first.
func (t *Terms) List(kind Kind) []string { return t.lists[kind] }

// Has reports whether a term is present.
func (t *Terms) Has(kind Kind, value string) bool { return t.set[kind][value] }

// FingerprintVersion identifies the vocabulary the company fingerprint
// depends on.
//
// Only legal forms and connectors take part: they decide what a company
// name reduces to, and the reduction is baked into a stored fingerprint.
// Changing either means already-stored vacancies would fingerprint
// differently today than they did when saved, and dedup would stop
// recognising them. Recording this hash on every row makes that visible.
func (t *Terms) FingerprintVersion() string {
	var b strings.Builder
	for _, kind := range []Kind{LegalForm, Connector} {
		values := append([]string(nil), t.lists[kind]...)
		sort.Strings(values) // order of the hash must not depend on length sorting
		b.WriteString(string(kind))
		b.WriteByte(':')
		b.WriteString(strings.Join(values, ","))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:12]
}

// Default is the vocabulary compiled into the binary. It seeds the table
// and stands in wherever no database is available, so the same matching
// behaviour is available offline.
func Default() *Terms {
	return New(map[Kind][]string{
		LegalForm: {
			"gmbh co kg", "ag co kg", "gmbh co", "ug haftungsbeschraenkt",
			"gmbh", "mbh", "ug", "ag", "se", "kg", "ohg", "ev",
			"ltd", "limited", "inc", "llc", "plc",
			"bv", "nv", "sa", "sarl", "oy", "ab", "a s",
		},
		Connector: {"und", "and", "et"},
		Softener: {
			"idealerweise", "von vorteil", "wuenschenswert", "wünschenswert",
			"pluspunkt", "gerne auch", "bonus", "nice to have", "a plus",
			"optional", "desirable", "preferred", "would be great",
		},
		LangMarkerDE: {
			" und ", " oder ", " nicht ", " eine ", " wir ", " sie ",
			" mit ", " fuer ", " für ", " bei ", " unser ", " werden ",
			" haben ", " sind ",
		},
		LangMarkerEN: {
			" and ", " or ", " the ", " you ", " we ", " with ",
			" for ", " your ", " our ", " will ", " have ", " are ",
		},
	})
}
