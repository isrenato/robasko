package protect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isrenato/robasko/internal/scrub"
)

func matcher(t *testing.T, targets []scrub.Target) *scrub.Matcher {
	t.Helper()
	m, err := scrub.NewMatcher(targets)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	return m
}

func TestParseListReadsCategories(t *testing.T) {
	in := `
# the maintainer
person:   Jürgen Müller
locality: Steinbachtal
email:    jm@example.invalid
phone:    +49 561 7788990

`
	got, err := ParseList(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d targets, want 4: %+v", len(got), got)
	}
	if got[1].Category != scrub.Address {
		t.Errorf("locality did not map to the address category: %+v", got[1])
	}
}

func TestParseListRejectsMalformedLines(t *testing.T) {
	if _, err := ParseList(strings.NewReader("no colon here")); err == nil {
		t.Error("expected a malformed line to be rejected")
	}
	if _, err := ParseList(strings.NewReader("wizard: Gandalf")); err == nil {
		t.Error("expected an unknown category to be rejected")
	}
}

func TestScanRejectsContentCarryingALiteral(t *testing.T) {
	m := matcher(t, []scrub.Target{{Value: "Jürgen Müller", Category: scrub.Person}})

	got := Scan(m, map[string]string{"README.md": "Written by Jürgen Müller."})
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Path != "README.md" {
		t.Errorf("finding names the wrong path: %q", got[0].Path)
	}
	if !strings.Contains(got[0].String(), "person") {
		t.Errorf("finding does not report the matched rule: %s", got[0])
	}
}

func TestScanPassesCleanContent(t *testing.T) {
	m := matcher(t, []scrub.Target{{Value: "Jürgen Müller", Category: scrub.Person}})

	if got := Scan(m, map[string]string{"README.md": "Nothing to see."}); len(got) != 0 {
		t.Errorf("clean content produced findings: %+v", got)
	}
}

// The check must agree with the scrubber: a spelling the scrubber would have
// replaced is a spelling the check has to catch.
func TestScanMatchesDiacriticAndGroupingVariants(t *testing.T) {
	m := matcher(t, []scrub.Target{
		{Value: "Jürgen Müller", Category: scrub.Person},
		{Value: "+49 561 7788990", Category: scrub.Phone},
	})

	cases := map[string]string{
		"plain":          "Jurgen Muller wrote this",
		"transliterated": "Juergen Mueller wrote this",
		"upper":          "JÜRGEN MÜLLER wrote this",
		"phone-dashes":   "call +49-561-77-88-990 today",
		"phone-parens":   "call +49 (561) 7788990 today",
		"phone-tight":    "call 00495617788990 today",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Scan(m, map[string]string{"f.md": content}); len(got) == 0 {
				t.Errorf("variant slipped through: %q", content)
			}
		})
	}
}

func TestLoadTargetsReportsAbsentListWithoutError(t *testing.T) {
	_, found, err := LoadTargets(ListPath(t.TempDir()))
	if err != nil {
		t.Fatalf("an absent list must not be an error: %v", err)
	}
	if found {
		t.Error("reported a list that does not exist")
	}
}

func TestLoadTargetsReadsAnExistingList(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(ListPath(root), []byte("person: Jürgen Müller\n"), 0o600); err != nil {
		t.Fatalf("writing list: %v", err)
	}

	targets, found, err := LoadTargets(ListPath(root))
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if !found {
		t.Fatal("existing list reported as absent")
	}
	if len(targets) != 1 || targets[0].Value != "Jürgen Müller" {
		t.Errorf("unexpected targets: %+v", targets)
	}
}

func TestListPathSitsInTheDataRoot(t *testing.T) {
	root := t.TempDir()
	if got, want := ListPath(root), filepath.Join(root, ListFileName); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
