package letters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Letter statuses, matching the check on the letters table.
const (
	StatusDraft      = "draft"
	StatusNeedsHuman = "needs_human"
)

// MaxShortenRetries is SPEC.md §13.5's "at most twice".
const MaxShortenRetries = 2

// Errors the renderer distinguishes.
var (
	// ErrTooLong reports a letter that does not fit one page. It is the
	// renderer's exit code 2, and the only one worth reacting to: the
	// caller shortens the draft and tries again.
	ErrTooLong = errors.New("the letter does not fit one page")
	// ErrNoRenderer reports that python or the script is not there. The
	// runtime image is distroless and has neither, so this is an ordinary
	// state and not a failure: the .txt is still written, and the letter
	// is simply a draft without a PDF beside it.
	ErrNoRenderer = errors.New("the PDF renderer is not available")
)

// PDF is everything the renderer needs that is not in the letter body.
type PDF struct {
	// Python is the interpreter, "python3" when empty.
	Python string
	// Script is the path to scripts/build_letter.py. An empty path means
	// no PDF is wanted, which is how a caller with no python says so.
	Script string

	Subject   string
	Company   string
	Recipient []string

	SenderName  string
	SenderLines []string
	Date        string
}

// Build renders one .txt into one .pdf.
func (p PDF) Build(ctx context.Context, textPath, outPath string) error {
	if p.Script == "" {
		return ErrNoRenderer
	}
	if _, err := os.Stat(p.Script); err != nil {
		return fmt.Errorf("%w: %s", ErrNoRenderer, p.Script)
	}

	python := p.Python
	if python == "" {
		python = "python3"
	}
	bin, err := exec.LookPath(python)
	if err != nil {
		return fmt.Errorf("%w: %s is not on the path", ErrNoRenderer, python)
	}

	// -I isolates the interpreter: no site-packages, no PYTHONPATH, and
	// no module picked up from the directory the script happens to sit
	// in. The script needs nothing but the standard library, so there is
	// nothing to lose by refusing the rest.
	args := []string{"-I", p.Script, "--text", textPath, "--out", outPath}
	for _, pair := range [][2]string{
		{"--subject", p.Subject}, {"--company", p.Company},
		{"--sender-name", p.SenderName}, {"--date", p.Date},
	} {
		if pair[1] != "" {
			args = append(args, pair[0], pair[1])
		}
	}
	for _, line := range p.Recipient {
		args = append(args, "--recipient", line)
	}
	for _, line := range p.SenderLines {
		args = append(args, "--sender-line", line)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err == nil {
		return nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		return fmt.Errorf("%w: %s", ErrTooLong, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("letters: rendering %s: %w: %s",
		filepath.Base(textPath), err, strings.TrimSpace(stderr.String()))
}

// Rendered is what came out of a drafting run.
type Rendered struct {
	// TxtPath and PDFPath are as written, below dir. The letters table
	// stores them relative to the working tree, which is the caller's to
	// do: an absolute path in that column records a home directory.
	TxtPath string
	PDFPath string
	// Cuts names each thing the one-page retry removed, in order. A
	// letter that needed cutting is one whose style targets are wrong.
	Cuts []string
	// Status is draft or needs_human.
	Status string
	Reason string
}

// Render writes the draft and renders it, shortening when it does not fit.
//
// SPEC.md §13.5: exit 2 means another page, which means the lowest-weighted
// paragraph loses material and the renderer runs again, at most twice.
// After that the letter is marked needs_human rather than cut further,
// because a letter that is still too long after two cuts is not a length
// problem.
//
// A missing renderer is not a failure of the letter. The .txt is written
// either way, and it is the .txt that carries the words.
func Render(ctx context.Context, d *Draft, dir, name string, p PDF, weight func(string) int) (Rendered, error) {
	if weight == nil {
		weight = func(string) int { return 0 }
	}
	out := Rendered{Status: StatusDraft}

	txtPath, err := Save(dir, name, d.Text())
	if err != nil {
		return out, err
	}
	out.TxtPath = txtPath

	pdfPath := strings.TrimSuffix(txtPath, filepath.Ext(txtPath)) + ".pdf"

	for attempt := 0; ; attempt++ {
		err := p.Build(ctx, txtPath, pdfPath)
		switch {
		case err == nil:
			out.PDFPath = pdfPath
			return out, nil

		case errors.Is(err, ErrNoRenderer):
			out.Reason = "no PDF: " + err.Error()
			return out, nil

		case !errors.Is(err, ErrTooLong):
			return out, err

		case attempt >= MaxShortenRetries:
			out.Status = StatusNeedsHuman
			out.Reason = fmt.Sprintf("still too long after %d cuts: %v", attempt, err)
			return out, nil
		}

		cut, ok := Shorten(d, weight)
		if !ok {
			out.Status = StatusNeedsHuman
			out.Reason = "too long, and there is nothing left to cut"
			return out, nil
		}
		out.Cuts = append(out.Cuts, cut)

		// Rewritten rather than appended to: the file on disk is what is
		// sent, and a stale one beside a fresh PDF is the worst of both.
		if _, err := Save(dir, name, d.Text()); err != nil {
			return out, err
		}
	}
}
