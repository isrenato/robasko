// Package letters drafts a cover letter and holds it to its evidence.
//
// The rule that matters here is the fact lock (SPEC.md §13.3): every claim
// carries an inventory id or a profile fact id, and a sentence with no
// source is rejected rather than softened. An invented fact is not caught
// by the reader; it is caught in the interview.
//
// The lock is built in two layers. The reply schema enumerates the ids
// that exist, so an invented id cannot be returned at all. Then every
// sentence is checked to carry at least one, so a sentence that cites
// nothing cannot pass by being plausible.
//
// Neither layer catches the third failure: a real id attached to a claim
// stronger than the evidence behind it. Nothing mechanical can. That is
// what the claims section of style.yaml is for, and why a draft is a
// draft until a human approves it.
package letters

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/llm"
	"github.com/isrenato/robasko/internal/profile"
	"github.com/isrenato/robasko/internal/scoring"
)

// Threshold is the score at or above which a letter may be drafted
// (SPEC.md §13).
const Threshold = 80.0

// Errors a caller is expected to distinguish.
var (
	// ErrBelowThreshold reports a vacancy that does not warrant a letter.
	ErrBelowThreshold = fmt.Errorf("score is below the letter threshold")
	// ErrMustGap reports an uncovered must-have. A letter cannot honestly
	// be written for one, and §13.4 relies on it being impossible.
	ErrMustGap = fmt.Errorf("the vacancy has an uncovered must-have")
	// ErrUnsourced reports a sentence the draft could not justify.
	ErrUnsourced = fmt.Errorf("a sentence carries no source")
)

// Vacancy is what a letter is written about.
type Vacancy struct {
	Title    string
	Company  string
	City     string
	Language string // "de" or "en"; the advertisement's language
	// Contact is the person named in the advertisement, where there is
	// one. The salutation depends on how much of it is known.
	ContactFirstName string
	ContactLastName  string
	ContactGender    string // "f", "m" or "" when the advertisement does not say
	AdText           string
}

// Sentence is one claim and the ids that support it.
type Sentence struct {
	Text    string   `json:"text"`
	Sources []string `json:"sources"`
}

// Paragraph is one section of the letter.
type Paragraph struct {
	ID        string     `json:"id"`
	Sentences []Sentence `json:"sentences"`
}

// Draft is a letter before a human has seen it.
type Draft struct {
	Salutation string      `json:"salutation"`
	Paragraphs []Paragraph `json:"paragraphs"`
	Closing    string      `json:"closing"`

	// FactsUsed is every id the letter cited, deduplicated and sorted.
	// SPEC.md §13.3 stores it beside the letter.
	FactsUsed []string `json:"-"`
}

// Text renders the draft as the letter body.
func (d *Draft) Text() string {
	var b strings.Builder
	b.WriteString(d.Salutation)
	b.WriteString("\n\n")
	for _, p := range d.Paragraphs {
		lines := make([]string, 0, len(p.Sentences))
		for _, s := range p.Sentences {
			lines = append(lines, strings.TrimSpace(s.Text))
		}
		b.WriteString(strings.Join(lines, " "))
		b.WriteString("\n\n")
	}
	b.WriteString(d.Closing)
	b.WriteString("\n")
	return b.String()
}

// Sources is everything a letter is allowed to cite.
type Sources struct {
	Inventory *inventory.Inventory
	Profile   *profile.Profile
}

// IDs returns every citable id, sorted, for the schema's enum.
func (s Sources) IDs() []string {
	var out []string
	for _, e := range s.Inventory.Entries {
		out = append(out, e.ID)
	}
	out = append(out, s.Profile.IDs()...)
	sort.Strings(out)
	return out
}

// Resolve returns the text behind an id, and whether it exists.
func (s Sources) Resolve(id string) (string, bool) {
	if e, ok := s.Inventory.Get(id); ok {
		return e.Evidence, true
	}
	if f, ok := s.Profile.Get(id); ok {
		return f.Text, true
	}
	return "", false
}

// Schema constrains the reply. The sources enum is the first layer of the
// fact lock: an id outside it cannot be returned.
func Schema(src Sources) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"salutation", "paragraphs", "closing"},
		"properties": map[string]any{
			"salutation": map[string]any{"type": "string", "minLength": 1},
			"closing":    map[string]any{"type": "string", "minLength": 1},
			"paragraphs": map[string]any{
				"type":     "array",
				"minItems": 2,
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"id", "sentences"},
					"properties": map[string]any{
						"id": map[string]any{"type": "string"},
						"sentences": map[string]any{
							"type":     "array",
							"minItems": 1,
							"items": map[string]any{
								"type":                 "object",
								"additionalProperties": false,
								"required":             []string{"text", "sources"},
								"properties": map[string]any{
									"text": map[string]any{"type": "string", "minLength": 1},
									"sources": map[string]any{
										"type":     "array",
										"minItems": 1,
										"items":    map[string]any{"type": "string", "enum": src.IDs()},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// Check enforces the fact lock on a draft.
//
// It runs after the schema rather than instead of it: the schema makes an
// invented id unrepresentable, and this makes an unsourced sentence
// unacceptable. It also fills FactsUsed.
func (d *Draft) Check(src Sources) error {
	seen := map[string]bool{}

	for _, p := range d.Paragraphs {
		for i, s := range p.Sentences {
			if strings.TrimSpace(s.Text) == "" {
				return fmt.Errorf("%w: paragraph %q sentence %d is empty", ErrUnsourced, p.ID, i)
			}
			if len(s.Sources) == 0 {
				return fmt.Errorf("%w: %q", ErrUnsourced, truncate(s.Text))
			}
			for _, id := range s.Sources {
				if _, ok := src.Resolve(id); !ok {
					return fmt.Errorf("%w: sentence %q cites unknown id %q",
						ErrUnsourced, truncate(s.Text), id)
				}
				seen[id] = true
			}
		}
	}
	if len(seen) == 0 {
		return fmt.Errorf("%w: the letter cites nothing", ErrUnsourced)
	}

	d.FactsUsed = make([]string, 0, len(seen))
	for id := range seen {
		d.FactsUsed = append(d.FactsUsed, id)
	}
	sort.Strings(d.FactsUsed)
	return nil
}

func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 60 {
		return s
	}
	return s[:59] + "…"
}

// Eligible reports whether a vacancy warrants a letter at all.
//
// Both conditions come from SPEC.md §13: the score must clear the
// threshold, and there must be no uncovered must-have. The second is not
// redundant - the cap keeps a must-gap below 80 today, but a letter that
// depends on a cap for its honesty depends on the cap never changing.
func Eligible(result scoring.Result) error {
	if len(result.MustGaps) > 0 {
		return fmt.Errorf("%w: %s", ErrMustGap, strings.Join(result.MustGaps, "; "))
	}
	if result.Score < Threshold {
		return fmt.Errorf("%w: %.1f < %.0f", ErrBelowThreshold, result.Score, Threshold)
	}
	return nil
}

// Write drafts a letter for one vacancy.
func Write(ctx context.Context, c llm.Client, v Vacancy, result scoring.Result, reqs []scoring.Requirement, src Sources, style string) (*Draft, *llm.Response, error) {
	if err := Eligible(result); err != nil {
		return nil, nil, err
	}
	if src.Inventory == nil || src.Profile == nil {
		return nil, nil, fmt.Errorf("letters: need both an inventory and a profile")
	}

	resp, err := c.Structured(ctx, llm.Request{
		System: systemPrompt(src, style),
		Prompt: vacancyPrompt(v, reqs),
		Schema: Schema(src),
		Effort: llm.EffortHigh,
	})
	if err != nil {
		return nil, resp, err
	}

	var d Draft
	if err := json.Unmarshal(resp.JSON, &d); err != nil {
		return nil, resp, fmt.Errorf("letters: decoding the draft: %w", err)
	}
	if err := d.Check(src); err != nil {
		return nil, resp, err
	}
	return &d, resp, nil
}
