// Package protect scans content for protected literals.
//
// The list of literals - the maintainer's name, email addresses, telephone
// numbers, home locality and the configured pseudonymisation targets - is
// itself personal data, so it is not committed. It lives in the private data
// root and this package reads it from there.
//
// Hashing the literals and committing the hashes instead was considered and
// rejected: a given name and a village of a few thousand people carry far too
// little entropy to survive a dictionary attack, so committed hashes would
// offer the appearance of protection without the substance.
package protect

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/isrenato/robasko/internal/scrub"
)

// ListFileName is the literal list's name within the private data root.
const ListFileName = "protected-literals.txt"

// Finding reports one protected literal occurring in one file.
type Finding struct {
	Path  string
	Match scrub.Match
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: matched %s rule %q (as %q)",
		f.Path, categoryName(f.Match.Category), f.Match.Value, f.Match.Found)
}

var categoryNames = map[string]scrub.Category{
	"person":   scrub.Person,
	"company":  scrub.Company,
	"address":  scrub.Address,
	"locality": scrub.Address,
	"email":    scrub.Email,
	"phone":    scrub.Phone,
}

func categoryName(c scrub.Category) string {
	switch c {
	case scrub.Person:
		return "person"
	case scrub.Company:
		return "company"
	case scrub.Address:
		return "address"
	case scrub.Email:
		return "email"
	case scrub.Phone:
		return "phone"
	default:
		return "unknown"
	}
}

// ListPath returns the literal list's location within a data root.
func ListPath(dataRoot string) string { return filepath.Join(dataRoot, ListFileName) }

// LoadTargets reads the literal list. The second return value is false when
// no list exists, which is not an error: a third party cloning the public
// repository has no protected literals to check against and must not be
// blocked from committing.
func LoadTargets(path string) ([]scrub.Target, bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("opening literal list: %w", err)
	}
	defer f.Close()

	targets, err := ParseList(f)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return targets, true, nil
}

// ParseList reads "category: value" lines, ignoring blanks and # comments.
func ParseList(r io.Reader) ([]scrub.Target, error) {
	var targets []scrub.Target
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, ok := strings.Cut(text, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"category: value\", got %q", line, text)
		}
		category, known := categoryNames[strings.ToLower(strings.TrimSpace(name))]
		if !known {
			return nil, fmt.Errorf("line %d: unknown category %q", line, strings.TrimSpace(name))
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		targets = append(targets, scrub.Target{Value: value, Category: category})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading literal list: %w", err)
	}
	return targets, nil
}

// Scan returns every protected literal found in the supplied files, ordered
// by path so that output is stable.
func Scan(m *scrub.Matcher, files map[string]string) []Finding {
	var out []Finding
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		for _, match := range m.Find(files[p]) {
			out = append(out, Finding{Path: p, Match: match})
		}
	}
	return out
}
