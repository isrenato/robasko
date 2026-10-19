// Package keyword extracts requirements by matching inventory aliases
// against the advertisement text.
//
// This is a stand-in for `vacancy.extract_requirements` (SPEC.md §11),
// which uses a model. It exists so the scoring pipeline can be exercised
// end to end without an API key, and it has one structural flaw the model
// does not: it can only find skills the inventory already names. A demand
// the inventory has no word for is invisible here, so it never lowers the
// score. Scores produced this way read high.
package keyword

import (
	"regexp"
	"sort"
	"strings"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/scoring"
)

// softeners mark a line as a preference rather than a demand. German
// advertisements are fairly consistent about these.
var softeners = []string{
	"idealerweise", "von vorteil", "wünschenswert", "wuenschenswert",
	"von großem vorteil", "pluspunkt", "gerne auch", "bonus",
	"nice to have", "a plus", "is a plus", "optional", "desirable",
	"preferred", "would be great",
}

// Extract returns one requirement per inventory entry mentioned in text.
//
// The line carrying the mention becomes the requirement's verbatim text,
// which is what makes a score explainable: every point points back at a
// sentence someone actually wrote.
func Extract(text string, inv *inventory.Inventory) []scoring.Requirement {
	lines := splitLines(text)

	type hit struct {
		req   scoring.Requirement
		order int
	}
	found := map[string]*hit{}

	for i, line := range lines {
		lower := strings.ToLower(line)
		kind := scoring.Must
		if containsAny(lower, softeners) {
			kind = scoring.Nice
		}

		for j := range inv.Entries {
			e := &inv.Entries[j]
			if !mentions(line, e.Terms()) {
				continue
			}
			existing, seen := found[e.ID]
			if seen {
				// A skill named as a demand somewhere and as a
				// preference elsewhere is a demand.
				if existing.req.Kind == scoring.Nice && kind == scoring.Must {
					existing.req.Kind = scoring.Must
					existing.req.Text = line
				}
				continue
			}
			found[e.ID] = &hit{order: i, req: scoring.Requirement{
				Text:     line,
				Kind:     kind,
				IDs:      []string{e.ID},
				Coverage: e.Level.Coverage(),
				Evidence: e.Evidence,
			}}
		}
	}

	// Stable order: the order the advertisement mentions things in.
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		if found[ids[a]].order != found[ids[b]].order {
			return found[ids[a]].order < found[ids[b]].order
		}
		return ids[a] < ids[b]
	})

	out := make([]scoring.Requirement, 0, len(ids))
	for _, id := range ids {
		out = append(out, found[id].req)
	}
	return out
}

func splitLines(text string) []string {
	var out []string
	for _, raw := range regexp.MustCompile(`[\n·•]|(?:\. )`).Split(text, -1) {
		if s := strings.TrimSpace(raw); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

var termRe = map[string]*regexp.Regexp{}

// mentions reports whether any spelling appears as a whole word. Substring
// matching is useless here: "go" lives inside "Google" and "Cellagon".
func mentions(line string, terms []string) bool {
	for _, t := range terms {
		re, ok := termRe[t]
		if !ok {
			re = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}+#])` +
				regexp.QuoteMeta(t) + `($|[^\p{L}\p{N}+#])`)
			termRe[t] = re
		}
		if re.MatchString(line) {
			return true
		}
	}
	return false
}
