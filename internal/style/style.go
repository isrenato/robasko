// Package style loads the file that decides how a letter reads.
//
// The division of labour is the point. profile.yaml and inventory.yaml say
// what a letter may claim, and the fact lock checks every sentence against
// them. Nothing in this file is checked against anything: it shapes the
// prose, names the fixed lines, and sets how long the result may be.
//
// Two parts of it are read by code rather than by the model. The
// salutation and closing are rendered here and written into the draft
// afterwards, because a greeting is a template with slots and asking a
// model to fill one in is a way to get it wrong in a letter's most visible
// line. The per-section weight is read by the shortening retry, which has
// to know which paragraph to cut when the letter does not fit a page.
package style

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Style is the loaded file.
type Style struct {
	Sender        Sender              `yaml:"sender"`
	Languages     map[string]Language `yaml:"languages"`
	Length        Length              `yaml:"length"`
	Structure     []Section           `yaml:"structure"`
	Claims        []string            `yaml:"claims"`
	TiredOpenings []string            `yaml:"tired_openings"`
	VoiceSample   string              `yaml:"voice_sample"`
}

// Sender is who the letter is from. It appears in the rendered PDF and
// nowhere else: the body cites profile facts, not this.
type Sender struct {
	Name string `yaml:"name"`
	// Lines are the address block as it should be printed, one entry per
	// line. Left empty, the PDF carries the name alone, which is enough
	// for a letter sent as an email attachment.
	Lines []string `yaml:"lines"`
}

// Language holds the fixed lines for one language.
type Language struct {
	AddressAs  string     `yaml:"address_as"`
	Salutation Salutation `yaml:"salutation"`
	Closing    string     `yaml:"closing"`
	DateFormat string     `yaml:"date_format"`
	// Subject is the line above the salutation, with a {title} slot. It
	// is not part of the body the model writes: it is the same sentence
	// every time and only the role name changes.
	Subject string `yaml:"subject"`
}

// Salutation is the four forms from SPEC.md §13.2. The slots are
// {first_name}, {last_name} and {company}.
type Salutation struct {
	GenderedFemale string `yaml:"gendered_female"`
	GenderedMale   string `yaml:"gendered_male"`
	Ungendered     string `yaml:"ungendered"`
	NoContact      string `yaml:"no_contact"`
}

// Length is what the one-page rule aims at.
type Length struct {
	MaxPages    int   `yaml:"max_pages"`
	TargetWords int   `yaml:"target_words"`
	MaxWords    int   `yaml:"max_words"`
	Paragraphs  []int `yaml:"paragraphs"`
}

// Section is one paragraph the letter is built from.
type Section struct {
	ID string `yaml:"id"`
	// Weight is what the shortening retry reads: the lowest goes first,
	// so the paragraph carrying the evidence outranks the one explaining
	// motivation.
	Weight    int    `yaml:"weight"`
	Intent    string `yaml:"intent"`
	Sentences []int  `yaml:"sentences"`
	Optional  bool   `yaml:"optional"`
}

// Contact is who the letter is addressed to, as far as the advertisement
// says.
type Contact struct {
	FirstName string
	LastName  string
	// Gender is "f", "m", or empty when the advertisement does not say. An
	// empty one is addressed without guessing, which is the whole reason
	// the ungendered form exists.
	Gender  string
	Company string
}

// Load reads a style file.
func Load(path string) (*Style, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the style file: %w", err)
	}
	return Parse(raw)
}

// Parse reads a style from YAML.
func Parse(raw []byte) (*Style, error) {
	var s Style
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parsing the style file: %w", err)
	}
	if len(s.Languages) == 0 {
		return nil, fmt.Errorf("the style file lists no languages")
	}
	for code, lang := range s.Languages {
		if strings.TrimSpace(lang.Closing) == "" {
			return nil, fmt.Errorf("language %q has no closing line", code)
		}
		if strings.TrimSpace(lang.Salutation.NoContact) == "" {
			return nil, fmt.Errorf("language %q has no salutation for an advertisement without a contact", code)
		}
	}

	seen := map[string]bool{}
	for _, sec := range s.Structure {
		if sec.ID == "" {
			return nil, fmt.Errorf("a structure section has no id")
		}
		if seen[sec.ID] {
			return nil, fmt.Errorf("structure section %q appears twice", sec.ID)
		}
		seen[sec.ID] = true
	}
	return &s, nil
}

// placeholder marks the voice sample the example file ships with. It is
// instruction rather than voice, and feeding it to the model would teach
// it to write about writing.
const placeholder = "Replace this"

// Warnings reports what the file is missing without refusing to load it.
// A style file with no voice sample still produces a letter; it produces a
// blander one, and the operator should know that before reading the draft.
func (s *Style) Warnings() []string {
	var out []string
	if s.voiceSample() == "" {
		out = append(out, "voice_sample is unset or still the placeholder: the draft will not match your writing")
	}
	if strings.TrimSpace(s.Sender.Name) == "" {
		out = append(out, "sender.name is unset: the rendered letter will have no signature")
	}
	if len(s.Structure) == 0 {
		out = append(out, "no structure sections: the shortening retry has no weights and will cut the last paragraph")
	}
	return out
}

func (s *Style) voiceSample() string {
	v := strings.TrimSpace(s.VoiceSample)
	if v == "" || strings.HasPrefix(v, placeholder) {
		return ""
	}
	return v
}

// Lang returns the settings for a language, falling back to English and
// then to whatever the file does define. A letter in the wrong register is
// recoverable; a letter with no closing line is not.
func (s *Style) Lang(code string) Language {
	if l, ok := s.Languages[code]; ok {
		return l
	}
	if l, ok := s.Languages["en"]; ok {
		return l
	}
	codes := make([]string, 0, len(s.Languages))
	for c := range s.Languages {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	return s.Languages[codes[0]]
}

// Salutation renders the greeting for a contact.
//
// The four cases are SPEC.md §13.2's, and the third is the one that
// matters: a name whose gender the advertisement does not state is
// addressed with both names rather than with a guess. Guessing is wrong
// about some people every time, and it is wrong in the first line.
func (s *Style) Salutation(code string, c Contact) string {
	forms := s.Lang(code).Salutation

	var tmpl string
	switch {
	case c.LastName == "":
		tmpl = forms.NoContact
	case c.Gender == "f" && forms.GenderedFemale != "":
		tmpl = forms.GenderedFemale
	case c.Gender == "m" && forms.GenderedMale != "":
		tmpl = forms.GenderedMale
	default:
		tmpl = forms.Ungendered
	}
	if strings.TrimSpace(tmpl) == "" {
		tmpl = forms.NoContact
	}

	return strings.NewReplacer(
		"{first_name}", c.FirstName,
		"{last_name}", c.LastName,
		"{company}", c.Company,
	).Replace(strings.TrimSpace(tmpl))
}

// Closing returns the sign-off line for a language.
func (s *Style) Closing(code string) string {
	return strings.TrimSpace(s.Lang(code).Closing)
}

// Subject renders the subject line for a role.
func (s *Style) Subject(code, title string) string {
	tmpl := strings.TrimSpace(s.Lang(code).Subject)
	if tmpl == "" {
		// Not a translation worth inventing per language: a subject line
		// that is only the role still files correctly.
		return title
	}
	return strings.ReplaceAll(tmpl, "{title}", title)
}

// DateFormat returns the Go layout for the date line.
func (s *Style) DateFormat(code string) string {
	if f := strings.TrimSpace(s.Lang(code).DateFormat); f != "" {
		return f
	}
	return "2 January 2006"
}

// Weight returns a section's weight, and 0 for a section the file does not
// describe. Cutting an undescribed paragraph first is deliberate: the
// style file lists the paragraphs a letter is meant to have, so one that
// is not on the list is the one nobody asked for.
func (s *Style) Weight(sectionID string) int {
	for _, sec := range s.Structure {
		if sec.ID == sectionID {
			return sec.Weight
		}
	}
	return 0
}

// Prompt renders the part of the style file the model is shown.
//
// Everything structural is included and everything operational is left
// out: the model is told how long to write and in what register, not that
// a python script will check the length afterwards.
func (s *Style) Prompt() string {
	var b strings.Builder

	if s.Length.TargetWords > 0 || s.Length.MaxWords > 0 {
		b.WriteString("Length. ")
		if s.Length.TargetWords > 0 {
			fmt.Fprintf(&b, "Aim for about %d words", s.Length.TargetWords)
		}
		if s.Length.MaxWords > 0 {
			if s.Length.TargetWords > 0 {
				b.WriteString(" and ")
			}
			fmt.Fprintf(&b, "never exceed %d", s.Length.MaxWords)
		}
		b.WriteString(". The letter must fit one page.\n\n")
	}

	if len(s.Structure) > 0 {
		b.WriteString("Paragraphs, in this order. Use the id given; omit an optional\n")
		b.WriteString("paragraph rather than padding it.\n")
		for _, sec := range s.Structure {
			fmt.Fprintf(&b, "- %s", sec.ID)
			if len(sec.Sentences) == 2 {
				fmt.Fprintf(&b, " (%d-%d sentences)", sec.Sentences[0], sec.Sentences[1])
			}
			if sec.Optional {
				b.WriteString(" (optional)")
			}
			b.WriteString(": ")
			b.WriteString(strings.Join(strings.Fields(sec.Intent), " "))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(s.Claims) > 0 {
		b.WriteString("What may not be claimed.\n")
		for _, c := range s.Claims {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		b.WriteString("\n")
	}

	if len(s.TiredOpenings) > 0 {
		b.WriteString("Phrases that are worn out. Do not use these or anything close to them.\n")
		for _, p := range s.TiredOpenings {
			fmt.Fprintf(&b, "- %s\n", p)
		}
		b.WriteString("\n")
	}

	if v := s.voiceSample(); v != "" {
		b.WriteString("The candidate's own writing. Match its sentence length, punctuation\n")
		b.WriteString("and how it opens; do not reuse its wording.\n")
		b.WriteString(strings.Join(strings.Fields(v), " "))
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}
