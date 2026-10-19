// Command protect rejects content carrying a protected literal.
//
// Run without arguments it checks what is staged, which is how the
// pre-commit hook uses it. Run with -all it checks every tracked file, which
// is how the repository is audited as a whole.
//
// It is the last line, behind the data root and the default-deny ignore
// rules, and it is the only one that can see a file the other two let
// through - a fixture edited by hand, say, or a path added to the allowlist
// without being read first.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/isrenato/robasko/internal/paths"
	"github.com/isrenato/robasko/internal/protect"
	"github.com/isrenato/robasko/internal/scrub"
)

func main() {
	all := flag.Bool("all", false, "check every tracked file instead of the staged changes")
	flag.Parse()

	if err := run(*all); err != nil {
		fmt.Fprintf(os.Stderr, "protect: %v\n", err)
		os.Exit(2)
	}
}

func run(all bool) error {
	repoRoot, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("not inside a git repository: %w", err)
	}
	repoRoot = strings.TrimSpace(repoRoot)

	dataRoot, err := paths.DataRoot(repoRoot)
	if err != nil {
		return err
	}

	listPath := protect.ListPath(dataRoot)
	targets, found, err := protect.LoadTargets(listPath)
	if err != nil {
		return err
	}
	if !found {
		// A third party cloning the public repository has no protected
		// literals to check against, and must not be blocked from
		// committing because of it.
		fmt.Fprintf(os.Stderr,
			"protect: no literal list at %s; skipping the check.\n"+
				"protect: create it to enable the scan (see README, \"Protected literals\").\n",
			listPath)
		return nil
	}
	if len(targets) == 0 {
		fmt.Fprintf(os.Stderr, "protect: literal list at %s is empty; skipping the check.\n", listPath)
		return nil
	}

	matcher, err := scrub.NewMatcher(targets)
	if err != nil {
		return err
	}

	files, err := collect(all)
	if err != nil {
		return err
	}

	findings := protect.Scan(matcher, files)
	if len(findings) == 0 {
		return nil
	}

	scope := "staged changes"
	if all {
		scope = "tracked files"
	}
	fmt.Fprintf(os.Stderr, "\nprotect: refusing to proceed - protected literals found in the %s:\n\n", scope)
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "  %s\n", f)
	}
	fmt.Fprintf(os.Stderr, "\nRemove the values, or correct the list at %s.\n\n", listPath)
	os.Exit(1)
	return nil
}

// collect returns the contents to scan, keyed by repository path. Binary
// files are skipped: a protected literal in one would not be found by a text
// scan anyway, and decoding noise produces false positives.
func collect(all bool) (map[string]string, error) {
	var listing string
	var err error
	if all {
		listing, err = git("ls-files", "-z")
	} else {
		listing, err = git("diff", "--cached", "--name-only", "--diff-filter=ACM", "-z")
	}
	if err != nil {
		return nil, err
	}

	files := map[string]string{}
	for _, path := range strings.Split(listing, "\x00") {
		if path == "" || skipPath(path) {
			continue
		}
		var content string
		if all {
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				// A tracked file may be absent from the working tree.
				continue
			}
			content = string(raw)
		} else {
			content, err = git("show", ":"+path)
			if err != nil {
				continue
			}
		}
		if strings.ContainsRune(content, '\x00') {
			continue
		}
		files[path] = content
	}
	return files, nil
}

// skipPath reports paths that are tracked but are not ours to police.
//
// Vendored dependencies are third-party source: they routinely contain
// addresses and names in comments and fixtures, none of it the maintainer's,
// and a check that cries wolf on every build is a check people learn to
// ignore.
func skipPath(path string) bool {
	return path == "vendor" || strings.HasPrefix(path, "vendor/")
}

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
