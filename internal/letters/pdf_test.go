package letters

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// script locates the renderer and skips when there is nothing to run it
// with. The runtime image has no python, and neither does every machine
// this is checked out on.
func script(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on the path")
	}
	path, err := filepath.Abs(filepath.Join("..", "..", "scripts", "build_letter.py"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no renderer at %s", path)
	}
	return path
}

func pdfFor(t *testing.T) PDF {
	return PDF{
		Script:      script(t),
		Subject:     "Bewerbung als Backend Engineer",
		Company:     "Nordwind Logistik GmbH",
		Recipient:   []string{"Frau Weber", "Freiburg"},
		SenderName:  "Alex Beispiel",
		SenderLines: []string{"Alex Beispiel", "alex@example.org"},
		Date:        "20. Oktober 2026",
	}
}

func TestRenderWritesBothFiles(t *testing.T) {
	dir := t.TempDir()
	d := draft()

	out, err := Render(context.Background(), d, dir, "letter.txt", pdfFor(t), weights)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out.Status != StatusDraft {
		t.Errorf("status %q, want draft", out.Status)
	}
	if len(out.Cuts) != 0 {
		t.Errorf("a short letter was shortened: %v", out.Cuts)
	}

	text, err := os.ReadFile(out.TxtPath)
	if err != nil {
		t.Fatalf("reading the draft: %v", err)
	}
	for _, want := range []string{"Sehr geehrte Frau Weber,", "Produktivbetrieb", "Mit freundlichen Grüßen"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("the .txt is missing %q", want)
		}
	}

	// 0600: the file is a name, an employment history and the employer it
	// is addressed to.
	info, err := os.Stat(out.TxtPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the draft is mode %04o, want 0600", perm)
	}

	raw, err := os.ReadFile(out.PDFPath)
	if err != nil {
		t.Fatalf("reading the PDF: %v", err)
	}
	if !strings.HasPrefix(string(raw), "%PDF-") {
		t.Error("the PDF does not start with a PDF header")
	}
	if !strings.HasSuffix(strings.TrimSpace(string(raw)), "%%EOF") {
		t.Error("the PDF is not terminated")
	}
}

// long builds a draft that cannot fit a page, with enough material in the
// low-weighted paragraphs that cutting them is what makes it fit.
func long(sentencesPerParagraph int) *Draft {
	filler := strings.Repeat(
		"Bei Nordwind Logistik habe ich einen Sendungsverfolgungsdienst in Go neu gebaut "+
			"und den nächtlichen Batch durch eine Event-Queue ersetzt. ", 3)

	d := &Draft{Salutation: "Sehr geehrte Frau Weber,", Closing: "Mit freundlichen Grüßen"}
	for _, id := range []string{"opening", "evidence", "fit"} {
		p := Paragraph{ID: id}
		for i := 0; i < sentencesPerParagraph; i++ {
			p.Sentences = append(p.Sentences, Sentence{Text: filler, Sources: []string{"go"}})
		}
		d.Paragraphs = append(d.Paragraphs, p)
	}
	return d
}

// SPEC.md §13.5: exit 2 means another page, so the lowest-weighted
// paragraph loses material and the renderer runs again.
func TestRenderShortensUntilItFits(t *testing.T) {
	dir := t.TempDir()
	d := long(3)

	out, err := Render(context.Background(), d, dir, "letter.txt", pdfFor(t), weights)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(out.Cuts) == 0 {
		t.Fatal("an over-long letter was not shortened")
	}
	if out.Status != StatusDraft {
		t.Fatalf("status %q with reason %q, want draft", out.Status, out.Reason)
	}
	for _, cut := range out.Cuts {
		if strings.Contains(cut, "evidence") {
			t.Errorf("the evidence paragraph was cut before the lower-weighted ones: %v", out.Cuts)
		}
	}

	// The file on disk is what gets sent, so it has to be the shortened
	// version and not the one that failed to fit.
	text, err := os.ReadFile(out.TxtPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != d.Text() {
		t.Error("the saved .txt is not the version that fit; a stale file beside a fresh PDF is the worst of both")
	}
}

// After two cuts it stops cutting. A letter still too long at that point
// is not a length problem, and a human should look at it.
func TestRenderGivesUpAfterTwoCuts(t *testing.T) {
	dir := t.TempDir()

	out, err := Render(context.Background(), long(12), dir, "letter.txt", pdfFor(t), weights)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out.Status != StatusNeedsHuman {
		t.Errorf("status %q, want needs_human", out.Status)
	}
	if len(out.Cuts) != MaxShortenRetries {
		t.Errorf("made %d cuts, want %d", len(out.Cuts), MaxShortenRetries)
	}
	if out.Reason == "" {
		t.Error("needs_human with no reason recorded")
	}
	if out.PDFPath != "" {
		t.Error("a PDF path was reported for a letter that never rendered")
	}
	// The words still exist even though the PDF does not.
	if _, err := os.Stat(out.TxtPath); err != nil {
		t.Errorf("the draft was not written: %v", err)
	}
}

// No python, no script: the .txt is still written and the letter is still
// a draft. The runtime image is distroless, so this is the ordinary case
// there and not a failure.
func TestRenderWithoutARendererStillWritesTheDraft(t *testing.T) {
	dir := t.TempDir()

	out, err := Render(context.Background(), draft(), dir, "letter.txt", PDF{}, weights)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out.Status != StatusDraft {
		t.Errorf("status %q, want draft", out.Status)
	}
	if out.PDFPath != "" {
		t.Errorf("a PDF path was reported: %q", out.PDFPath)
	}
	if !strings.Contains(out.Reason, "no PDF") {
		t.Errorf("reason %q does not say why there is no PDF", out.Reason)
	}
	if _, err := os.Stat(out.TxtPath); err != nil {
		t.Errorf("the draft was not written: %v", err)
	}
}

func TestBuildReportsAMissingRendererDistinctly(t *testing.T) {
	err := PDF{Script: filepath.Join(t.TempDir(), "nothing.py")}.
		Build(context.Background(), "in.txt", "out.pdf")
	if !errors.Is(err, ErrNoRenderer) {
		t.Errorf("got %v, want ErrNoRenderer", err)
	}
}

// Exit 2 has to be distinguishable from every other failure, because it
// is the only one the caller reacts to by changing the letter.
func TestBuildReportsOverflowAsTooLong(t *testing.T) {
	dir := t.TempDir()
	path, err := Save(dir, "letter.txt", long(12).Text())
	if err != nil {
		t.Fatal(err)
	}

	err = pdfFor(t).Build(context.Background(), path, filepath.Join(dir, "letter.pdf"))
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("got %v, want ErrTooLong", err)
	}
	if !strings.Contains(err.Error(), "past the page") {
		t.Errorf("the error does not say by how much: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "letter.pdf")); err == nil {
		t.Error("a PDF was written for a letter that does not fit")
	}
}
