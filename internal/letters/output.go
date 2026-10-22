package letters

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileName is the name SPEC.md §13.5 gives a draft:
// cover-letter_<CompanyNoSpaces>_<LastName>_<FirstName>.txt
//
// The name is the candidate's, not the contact's. The file is what an
// employer receives and files, and a company already appears in it; a
// second person's name would make a folder of applications unsortable.
func FileName(company, senderName string) string {
	parts := strings.Fields(senderName)
	var first, last string
	switch len(parts) {
	case 0:
	case 1:
		last = parts[0]
	default:
		first, last = strings.Join(parts[:len(parts)-1], ""), parts[len(parts)-1]
	}

	name := "cover-letter"
	for _, part := range []string{company, last, first} {
		if slug := slug(part); slug != "" {
			name += "_" + slug
		}
	}
	return name + ".txt"
}

// transliterations keep a German name readable once the non-ASCII is
// gone. Dropping the diacritic outright turns Müller into Mller, which is
// a worse filename than Mueller and a worse one to receive.
var transliterations = strings.NewReplacer(
	"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
	"Ä", "Ae", "Ö", "Oe", "Ü", "Ue",
	"é", "e", "è", "e", "ê", "e", "á", "a", "à", "a", "â", "a",
	"í", "i", "ì", "i", "ó", "o", "ò", "o", "ô", "o", "ú", "u", "ù", "u",
	"ç", "c", "ñ", "n", "å", "a", "ø", "o", "æ", "ae",
	"É", "E", "È", "E", "Á", "A", "À", "A", "Í", "I", "Ó", "O", "Ú", "U",
	"Ç", "C", "Ñ", "N", "Å", "A", "Ø", "O", "Æ", "Ae",
)

// slug reduces a name to the part that is safe in a filename on any of
// the three platforms this might run on.
func slug(s string) string {
	s = transliterations.Replace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Save writes a draft to dir and returns the path.
//
// 0600, because the file is a biography: a name, an employment history,
// and the employer it is addressed to. The directory is paths.Letters's
// to produce, and it refuses to produce one git would track.
func Save(dir, name, text string) (string, error) {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", fmt.Errorf("letters: writing %s: %w", path, err)
	}
	return path, nil
}

// Shorten removes the least valuable material from a draft and says what
// it removed.
//
// This is what SPEC.md §13.5 means by shortening the lowest-weighted
// paragraph after the renderer reports the letter does not fit. It cuts
// rather than rewrites, and deliberately so: a second model call to
// shorten a letter is a second chance to invent something, and the fact
// lock would have to run again on prose nobody asked for. Removing a
// sentence can only make the letter weaker, never less true.
//
// weight comes from the style file. The lowest wins, and a tie goes to
// the later paragraph, which keeps the opening intact when the file gives
// no weights at all.
func Shorten(d *Draft, weight func(sectionID string) int) (string, bool) {
	if len(d.Paragraphs) == 0 {
		return "", false
	}

	pick := -1
	for i, p := range d.Paragraphs {
		if pick == -1 || weight(p.ID) <= weight(d.Paragraphs[pick].ID) {
			pick = i
		}
	}

	p := &d.Paragraphs[pick]
	var cut string
	switch {
	case len(p.Sentences) > 1:
		// The last sentence of a paragraph is the one that elaborates;
		// the first is the one that makes the point.
		cut = fmt.Sprintf("the last sentence of %q", p.ID)
		p.Sentences = p.Sentences[:len(p.Sentences)-1]
	case len(d.Paragraphs) > 2:
		cut = fmt.Sprintf("the %q paragraph", p.ID)
		d.Paragraphs = append(d.Paragraphs[:pick], d.Paragraphs[pick+1:]...)
	default:
		// Two paragraphs of one sentence each is no longer a letter, and
		// cutting further would produce one that cannot be sent either.
		return "", false
	}

	d.refreshFacts()
	return cut, true
}

// refreshFacts recomputes FactsUsed after a cut, so the row stored beside
// the letter lists what the letter still cites rather than what an
// earlier version did.
func (d *Draft) refreshFacts() {
	seen := map[string]bool{}
	for _, p := range d.Paragraphs {
		for _, s := range p.Sentences {
			for _, id := range s.Sources {
				seen[id] = true
			}
		}
	}
	d.FactsUsed = make([]string, 0, len(seen))
	for id := range seen {
		d.FactsUsed = append(d.FactsUsed, id)
	}
	sort.Strings(d.FactsUsed)
}

// Words counts the body, for the length targets in the style file.
func (d *Draft) Words() int {
	n := 0
	for _, p := range d.Paragraphs {
		for _, s := range p.Sentences {
			n += len(strings.Fields(s.Text))
		}
	}
	return n
}
