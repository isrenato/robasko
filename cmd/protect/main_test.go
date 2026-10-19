package main

import "testing"

func TestSkipPathExcludesVendoredCode(t *testing.T) {
	skipped := []string{
		"vendor",
		"vendor/github.com/jackc/pgx/v5/pgtype/hstore.go",
		"vendor/modules.txt",
	}
	for _, p := range skipped {
		if !skipPath(p) {
			t.Errorf("%q should be skipped", p)
		}
	}

	kept := []string{
		"README.md",
		"internal/scrub/scrub.go",
		"cmd/agentd/main.go",
		// A path that merely begins with the same letters is ours.
		"vendored-notes.md",
	}
	for _, p := range kept {
		if skipPath(p) {
			t.Errorf("%q should not be skipped", p)
		}
	}
}
