package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestReadsNumberedSlotsInOrder(t *testing.T) {
	got, err := Sources(lookup(map[string]string{
		"SOURCE_1_NAME": "first", "SOURCE_1_BASE_URL": "https://a.example/api",
		"SOURCE_2_NAME": "second", "SOURCE_2_BASE_URL": "https://b.example/api",
		"SOURCE_2_API_KEY": "k", "SOURCE_2_API_ID": "i",
	}))
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" {
		t.Fatalf("unexpected sources: %+v", got)
	}
	if got[1].APIKey != "k" || got[1].APIID != "i" {
		t.Errorf("credentials not read: %+v", got[1])
	}
}

// A gap in the numbering is not an error: slots are a convenience, not a
// sequence anyone maintains carefully.
func TestSkipsEmptySlots(t *testing.T) {
	got, err := Sources(lookup(map[string]string{
		"SOURCE_1_NAME": "a", "SOURCE_1_BASE_URL": "https://a.example",
		"SOURCE_4_NAME": "d", "SOURCE_4_BASE_URL": "https://d.example",
	}))
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestHalfConfiguredSlotIsAnError(t *testing.T) {
	for _, m := range []map[string]string{
		{"SOURCE_1_BASE_URL": "https://a.example"},
		{"SOURCE_1_NAME": "a"},
	} {
		if _, err := Sources(lookup(m)); err == nil {
			t.Errorf("expected an error for %v", m)
		}
	}
}

func TestRejectsBaseURLWithoutScheme(t *testing.T) {
	_, err := Sources(lookup(map[string]string{
		"SOURCE_1_NAME": "a", "SOURCE_1_BASE_URL": "a.example/api",
	}))
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Errorf("got %v, want a scheme complaint", err)
	}
}

// The name is stored against every vacancy, so two sources sharing one
// would silently merge their results.
func TestRejectsDuplicateNames(t *testing.T) {
	_, err := Sources(lookup(map[string]string{
		"SOURCE_1_NAME": "same", "SOURCE_1_BASE_URL": "https://a.example",
		"SOURCE_2_NAME": "same", "SOURCE_2_BASE_URL": "https://b.example",
	}))
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("got %v, want a duplicate-name error", err)
	}
}

func TestNoSourcesIsAnError(t *testing.T) {
	if _, err := Sources(lookup(map[string]string{})); err == nil {
		t.Error("expected an error when nothing is configured")
	}
}

func TestTrimsTrailingSlashFromBaseURL(t *testing.T) {
	got, _ := Sources(lookup(map[string]string{
		"SOURCE_1_NAME": "a", "SOURCE_1_BASE_URL": "https://a.example/api/",
	}))
	if got[0].BaseURL != "https://a.example/api" {
		t.Errorf("got %q", got[0].BaseURL)
	}
}

func TestLoadFileReadsKeyValuePairs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.lock")
	body := "# comment\n\nSOURCE_1_NAME=board\nSOURCE_1_BASE_URL=\"https://a.example/api\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	look, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := look("SOURCE_1_NAME"); got != "board" {
		t.Errorf("got %q", got)
	}
	if got := look("SOURCE_1_BASE_URL"); got != "https://a.example/api" {
		t.Errorf("quotes not stripped: %q", got)
	}
}

func TestLoadFileRejectsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.lock")
	os.WriteFile(path, []byte("not a pair\n"), 0o600)

	if _, err := LoadFile(path); err == nil {
		t.Error("expected a malformed line to be rejected")
	}
}
