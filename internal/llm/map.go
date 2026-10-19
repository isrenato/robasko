package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/isrenato/robasko/internal/inventory"
	"github.com/isrenato/robasko/internal/scoring"
)

// mapSystemPreamble is the frozen part of the mapping instruction. The
// inventory follows it, which keeps the whole system prompt stable across
// every vacancy scored against one inventory version - and invalidates the
// cache exactly when the inventory changes, which is correct.
const mapSystemPreamble = `You match job requirements to entries in a skills inventory.

For each requirement you are given, list the inventory ids that cover it.

Rules:
- Use only ids from the inventory below. There is no id outside that list.
- A requirement nothing in the inventory covers gets an empty list. That is
  the expected answer for a demand the inventory has no word for, and it is
  more useful than a loose match.
- Match on meaning, not spelling: "Golang" is the go entry, "Postgres" and
  "PostgreSQL" are one entry, "k8s" is kubernetes.
- A requirement naming several technologies maps to several ids.
- Do not map a requirement to a related-but-different entry. Kafka is not
  RabbitMQ; MongoDB is not PostgreSQL. A wrong match is worse than none,
  because it turns a gap into apparent coverage.

Inventory:
`

// MapSchema constrains the reply to ids that exist.
//
// The enum is the whole point: an id outside the inventory cannot be
// returned at all, rather than being returned and rejected afterwards.
// SPEC.md §11 asks for that rejection; making it unrepresentable is
// stronger.
func MapSchema(inv *inventory.Inventory) map[string]any {
	ids := make([]string, 0, len(inv.Entries))
	for _, e := range inv.Entries {
		ids = append(ids, e.ID)
	}
	sort.Strings(ids)

	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"mappings"},
		"properties": map[string]any{
			"mappings": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"index", "inventory_ids"},
					"properties": map[string]any{
						"index": map[string]any{
							"type":        "integer",
							"minimum":     0,
							"description": "position of the requirement in the list given",
						},
						"inventory_ids": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string", "enum": ids},
						},
					},
				},
			},
		},
	}
}

// Mapping is one requirement's answer.
type Mapping struct {
	Index int      `json:"index"`
	IDs   []string `json:"inventory_ids"`
}

type mappingReply struct {
	Mappings []Mapping `json:"mappings"`
}

// MapRequirements turns extracted requirements into scored ones by
// attaching inventory ids and the coverage they carry.
//
// This is the step between extraction and scoring: extraction reads the
// advertisement and knows nothing about the candidate, scoring computes a
// number and knows nothing about language. Mapping is where the two meet,
// and it is the only place a requirement can be silently turned into
// coverage that is not there.
func MapRequirements(ctx context.Context, c Client, reqs []Requirement, inv *inventory.Inventory) ([]scoring.Requirement, *Response, error) {
	if len(reqs) == 0 {
		return nil, nil, nil
	}
	if inv == nil || len(inv.Entries) == 0 {
		return nil, nil, fmt.Errorf("llm: mapping needs an inventory")
	}

	resp, err := c.Structured(ctx, Request{
		System: mapSystemPreamble + describeInventory(inv),
		Prompt: "Requirements:\n\n" + numberRequirements(reqs),
		Schema: MapSchema(inv),
		Effort: EffortMedium,
	})
	if err != nil {
		return nil, resp, err
	}

	var reply mappingReply
	if err := json.Unmarshal(resp.JSON, &reply); err != nil {
		return nil, resp, fmt.Errorf("llm: decoding the mapping: %w", err)
	}

	byIndex := make(map[int][]string, len(reply.Mappings))
	for _, m := range reply.Mappings {
		if m.Index < 0 || m.Index >= len(reqs) {
			return nil, resp, fmt.Errorf("llm: mapping refers to requirement %d of %d", m.Index, len(reqs))
		}
		byIndex[m.Index] = m.IDs
	}

	out := make([]scoring.Requirement, 0, len(reqs))
	for i, r := range reqs {
		mapped := scoring.Requirement{
			Text: r.Text,
			Kind: scoring.Kind(r.Kind),
		}
		// A requirement the model said nothing about is unmapped, the same
		// as one it mapped to nothing. Both score zero; neither is an error.
		for _, id := range byIndex[i] {
			entry, ok := inv.Get(id)
			if !ok {
				// Unreachable through the enum, but the enum is enforced by
				// the server and this is the last line before a wrong id
				// becomes coverage.
				return nil, resp, fmt.Errorf("llm: mapping returned unknown id %q", id)
			}
			mapped.IDs = append(mapped.IDs, id)
			// SPEC.md §11: several ids take the highest coverage.
			if c := entry.Level.Coverage(); c > mapped.Coverage {
				mapped.Coverage = c
				mapped.Evidence = entry.Evidence
			}
		}
		out = append(out, mapped)
	}
	return out, resp, nil
}

// describeInventory renders the entries the model may choose from. Only the
// id, what it is and its aliases: the level is deliberately withheld, so
// the model cannot be tempted to map generously towards a strong entry or
// shy away from a weak one.
func describeInventory(inv *inventory.Inventory) string {
	lines := make([]string, 0, len(inv.Entries))
	for _, e := range inv.Entries {
		line := "- " + e.ID
		if e.Kind != "" {
			line += " (" + e.Kind + ")"
		}
		if aliases := e.Terms(); len(aliases) > 1 {
			line += ": " + strings.Join(aliases[1:], ", ")
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func numberRequirements(reqs []Requirement) string {
	var b strings.Builder
	for i, r := range reqs {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i, r.Kind, r.Text)
	}
	return b.String()
}
