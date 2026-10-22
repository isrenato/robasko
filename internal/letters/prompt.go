package letters

import (
	"fmt"
	"sort"
	"strings"

	"github.com/isrenato/robasko/internal/scoring"
)

// instructions is the frozen part of the system prompt. The sources and
// the style follow it, which keeps the whole prefix stable across every
// vacancy written against one profile and one style file.
const instructions = `You write cover letters on behalf of the candidate described below.

The hard rule: every sentence must cite at least one source id from the
list, and may only claim what that source supports. This is checked. A
sentence you cannot source is a sentence you must not write - write a
weaker true one instead, or leave it out.

What that means in practice:
- Never name a technology the candidate's sources do not cover.
- Never state a level above what a source records. A skill the sources
  describe as led rather than written is described as leading the work.
- Use a figure only where a source carries one, and use it as written.
- Claims about character - motivated, passionate, a team player - have no
  source and cannot be made.
- Do not mention anything the advertisement asked for that the candidate
  lacks. Omit it; do not apologise for it.
- Do not mention the score, the matching, or that this letter was drafted
  by a program.

Write in the language of the advertisement. Answer the advertisement's own
requirements rather than listing the candidate's history: a requirement
the advertisement did not raise is not worth a sentence.`

func systemPrompt(src Sources, style string) string {
	var b strings.Builder
	b.WriteString(instructions)

	if strings.TrimSpace(style) != "" {
		b.WriteString("\n\n--- How to write it ---\n")
		b.WriteString(strings.TrimSpace(style))
	}

	b.WriteString("\n\n--- Sources you may cite ---\n")
	b.WriteString("Skills. Cite the id; the note after it is the evidence.\n")
	for _, e := range src.Inventory.Entries {
		if e.Evidence == "" {
			// An entry with no evidence supports no claim, so it is not
			// offered as a source at all.
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", e.ID, e.Evidence)
	}

	b.WriteString("\nFacts. Cite the id; use the wording as given only where it says verbatim.\n")
	for _, f := range src.Profile.Facts {
		mark := ""
		if !f.Verbatim {
			mark = " [rephrase, do not quote]"
		}
		fmt.Fprintf(&b, "- %s: %s%s\n", f.ID, strings.Join(strings.Fields(f.Text), " "), mark)
	}
	return b.String()
}

// vacancyPrompt carries everything specific to this letter, and therefore
// goes last: it is the part that differs between calls, and the prefix
// above it stays cached.
func vacancyPrompt(v Vacancy, reqs []scoring.Requirement) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Role: %s\nCompany: %s\n", v.Title, v.Company)
	if v.City != "" {
		fmt.Fprintf(&b, "Location: %s\n", v.City)
	}
	if v.Language != "" {
		fmt.Fprintf(&b, "Language of the advertisement: %s\n", v.Language)
	}

	if v.Salutation != "" {
		fmt.Fprintf(&b, "\nSalutation, to be used exactly as written: %s\n", v.Salutation)
	} else {
		b.WriteString("\nSalutation to use: ")
		b.WriteString(salutationRule(v))
		b.WriteString("\n")
	}
	if v.Closing != "" {
		fmt.Fprintf(&b, "Closing, to be used exactly as written: %s\n", v.Closing)
	}

	// Only the covered requirements are offered. The uncovered ones are
	// withheld rather than marked, so there is nothing for the letter to
	// apologise for and no list to read back.
	covered := make([]scoring.Requirement, 0, len(reqs))
	for _, r := range reqs {
		if r.Coverage > 0 {
			covered = append(covered, r)
		}
	}
	sort.SliceStable(covered, func(i, j int) bool { return covered[i].Coverage > covered[j].Coverage })

	if len(covered) > 0 {
		b.WriteString("\nRequirements this candidate answers, strongest first. ")
		b.WriteString("Each names the source ids that cover it.\n")
		for _, r := range covered {
			fmt.Fprintf(&b, "- %s  [%s]\n", strings.Join(strings.Fields(r.Text), " "),
				strings.Join(r.IDs, ", "))
		}
	}

	if v.AdText != "" {
		b.WriteString("\n--- The advertisement ---\n")
		b.WriteString(strings.TrimSpace(v.AdText))
		b.WriteString("\n")
	}
	return b.String()
}

// salutationRule states which form to use. SPEC.md §13.2 gives three
// cases, and the distinction that matters is the middle one: a name whose
// gender the advertisement does not state is addressed without guessing.
func salutationRule(v Vacancy) string {
	switch {
	case v.ContactLastName == "":
		return `no contact is named - address the company team`
	case v.ContactGender == "f":
		return fmt.Sprintf(`the formal feminine form with the surname %q`, v.ContactLastName)
	case v.ContactGender == "m":
		return fmt.Sprintf(`the formal masculine form with the surname %q`, v.ContactLastName)
	default:
		return fmt.Sprintf(
			`%q %q - the advertisement does not give this person's gender, so use the neutral form with both names and do not guess one`,
			v.ContactFirstName, v.ContactLastName)
	}
}
