// Package profile loads the citable facts a letter may claim.
//
// The inventory says what the candidate can do; this says everything else
// a letter might assert - where they worked, what they shipped, what they
// studied. Both are addressable by id, because the fact lock records the
// ids a letter cited and a claim with no id is a claim with no source.
//
// Nothing here writes to the file. A model that can extend its own
// evidence base can talk itself into a qualification.
package profile

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind groups facts by what they assert.
type Kind string

const (
	Employment   Kind = "employment"
	Achievement  Kind = "achievement"
	Education    Kind = "education"
	Personal     Kind = "personal"
	Availability Kind = "availability"
)

// Fact is one citable claim.
type Fact struct {
	ID   string `yaml:"id"`
	Kind Kind   `yaml:"kind"`
	Text string `yaml:"text"`
	// Verbatim marks a fact a letter may use as worded. A fact that is not
	// verbatim still supports a claim, but the wording has to be the
	// letter's own - usually because the stored phrasing is shorthand that
	// would overstate if quoted.
	Verbatim bool `yaml:"verbatim"`
	// Until is the year after which the fact needs rechecking. A tenure
	// counted in years goes stale by sitting still.
	Until int `yaml:"until"`
}

// Profile is the loaded file, indexed by id.
type Profile struct {
	Facts []Fact
	byID  map[string]*Fact
}

var validKinds = map[Kind]bool{
	Employment: true, Achievement: true, Education: true,
	Personal: true, Availability: true,
}

// Load reads a profile file.
func Load(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the profile: %w", err)
	}
	return Parse(raw)
}

// Parse reads a profile from YAML.
func Parse(raw []byte) (*Profile, error) {
	var facts []Fact
	if err := yaml.Unmarshal(raw, &facts); err != nil {
		return nil, fmt.Errorf("parsing the profile: %w", err)
	}
	if len(facts) == 0 {
		return nil, fmt.Errorf("the profile is empty")
	}

	p := &Profile{Facts: facts, byID: make(map[string]*Fact, len(facts))}
	for i := range p.Facts {
		f := &p.Facts[i]
		if f.ID == "" {
			return nil, fmt.Errorf("fact %d has no id", i+1)
		}
		if strings.TrimSpace(f.Text) == "" {
			return nil, fmt.Errorf("fact %q has no text", f.ID)
		}
		if !validKinds[f.Kind] {
			return nil, fmt.Errorf("fact %q has an unknown kind %q", f.ID, f.Kind)
		}
		if _, dup := p.byID[f.ID]; dup {
			return nil, fmt.Errorf("fact %q appears twice", f.ID)
		}
		p.byID[f.ID] = f
	}
	return p, nil
}

// Get returns the fact with this id.
func (p *Profile) Get(id string) (*Fact, bool) {
	f, ok := p.byID[id]
	return f, ok
}

// IDs returns every fact id.
func (p *Profile) IDs() []string {
	out := make([]string, 0, len(p.Facts))
	for _, f := range p.Facts {
		out = append(out, f.ID)
	}
	return out
}

// Stale returns the facts whose `until` year has passed. A letter should
// not cite them until they are rechecked: "ten years of experience" is
// true for a year and then quietly is not.
func (p *Profile) Stale(year int) []Fact {
	var out []Fact
	for _, f := range p.Facts {
		if f.Until > 0 && year > f.Until {
			out = append(out, f)
		}
	}
	return out
}
