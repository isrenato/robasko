package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Kinds and categories a requirement can carry, from SPEC.md §11.
const (
	KindMust = "must"
	KindNice = "nice"
)

// Categories is the closed set a requirement is classified into. It is
// closed so the schema can reject anything else: an open set would let the
// model invent a category nobody aggregates on.
var Categories = []string{
	"language", "framework", "db", "cloud", "practice",
	"domain", "language_skill", "education", "location", "other",
}

// Requirement is one demand read off an advertisement.
type Requirement struct {
	// Text is quoted from the advertisement, not paraphrased: a score has
	// to point back at a sentence someone actually wrote.
	Text     string `json:"text"`
	Kind     string `json:"kind"`
	Category string `json:"category"`
}

// Extraction is the reply shape.
type Extraction struct {
	Requirements []Requirement `json:"requirements"`
}

// extractSystem is held byte-stable so the cached prefix survives between
// advertisements. Nothing per-vacancy belongs in it.
const extractSystem = `You read job advertisements and list what they require.

Rules:
- Quote each requirement verbatim from the advertisement. Do not paraphrase,
  translate, summarise or merge two requirements into one.
- kind is "must" when the advertisement states the requirement plainly, and
  "nice" when it softens it: idealerweise, von Vorteil, wuenschenswert,
  gerne, a plus, nice to have, optional, preferred.
- A negated or waived requirement is not a requirement. "Deutsch ist nicht
  erforderlich" and "no prior experience needed" are not listed at all.
- Boilerplate is not a requirement: company descriptions, benefits, equal
  opportunity statements, application instructions, and the job board's own
  footer.
- List a requirement once, even if the advertisement repeats it.
- Return an empty list rather than inventing requirements.`

// ExtractSchema is the shape the reply must satisfy.
func ExtractSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"requirements"},
		"properties": map[string]any{
			"requirements": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"text", "kind", "category"},
					"properties": map[string]any{
						"text":     map[string]any{"type": "string", "minLength": 1},
						"kind":     map[string]any{"type": "string", "enum": []string{KindMust, KindNice}},
						"category": map[string]any{"type": "string", "enum": Categories},
					},
				},
			},
		},
	}
}

// ExtractRequirements reads the requirements off one advertisement.
func ExtractRequirements(ctx context.Context, c Client, adText string) (*Extraction, *Response, error) {
	if strings.TrimSpace(adText) == "" {
		return nil, nil, fmt.Errorf("llm: empty advertisement")
	}

	resp, err := c.Structured(ctx, Request{
		System: extractSystem,
		// The advertisement goes last, after the frozen system prompt, so
		// the cached prefix is the same on every call.
		Prompt: "Advertisement:\n\n" + adText,
		Schema: ExtractSchema(),
		Effort: EffortMedium,
	})
	if err != nil {
		return nil, resp, err
	}

	var out Extraction
	if err := json.Unmarshal(resp.JSON, &out); err != nil {
		return nil, resp, fmt.Errorf("llm: decoding the extraction: %w", err)
	}

	// Checked rather than trusted: the schema constrains the reply, and a
	// mismatch here means the schema and this struct have drifted apart.
	for i, r := range out.Requirements {
		if strings.TrimSpace(r.Text) == "" {
			return nil, resp, fmt.Errorf("llm: requirement %d has no text", i)
		}
		if r.Kind != KindMust && r.Kind != KindNice {
			return nil, resp, fmt.Errorf("llm: requirement %d has kind %q", i, r.Kind)
		}
	}
	return &out, resp, nil
}
