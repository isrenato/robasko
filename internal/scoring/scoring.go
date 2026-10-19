// Package scoring turns a mapped requirement list into a number.
//
// No model runs here. The model reads the advertisement; the arithmetic is
// code, so every point traces back to one requirement and one inventory
// entry (SPEC.md §11).
package scoring

import "fmt"

// Kind separates what an advertisement demands from what it merely prefers.
type Kind string

const (
	Must Kind = "must"
	Nice Kind = "nice"
)

// Requirement is one demand from the advertisement after mapping.
type Requirement struct {
	Text     string   // verbatim from the advertisement
	Kind     Kind
	IDs      []string // inventory ids; empty means unmapped
	Coverage float64  // highest coverage among IDs; 0 when unmapped
	Evidence string
}

// Options carries the facts the caps need that the requirements do not
// carry themselves.
type Options struct {
	// LanguageShortfall is set when the advertisement asks for a language
	// level above the one the inventory records.
	LanguageShortfall bool
	// BeyondCommute is set when the post is onsite and further away than
	// the configured maximum.
	BeyondCommute bool
}

// Result is the score and everything needed to explain it.
type Result struct {
	Score        float64
	MustCoverage float64
	NiceCoverage float64
	MustCount    int
	NiceCount    int
	CappedReason string
	MustGaps     []string
	NiceGaps     []string
}

// Caps and their thresholds.
const (
	capMissingMust = 70.0
	capLanguage    = 75.0
	capCommute     = 60.0

	mustWeight = 0.8
	niceWeight = 0.2
)

// Compute scores a mapped requirement list.
//
// The two kinds are averaged over their own counts and then mixed at a
// fixed ratio. A single mean over everything would put the number at the
// mercy of how many items the advertisement happened to list, so a verbose
// employer would score lower than a terse one for the same real fit.
func Compute(reqs []Requirement, opts Options) Result {
	var r Result
	var mustSum, niceSum float64

	for _, req := range reqs {
		switch req.Kind {
		case Must:
			r.MustCount++
			mustSum += req.Coverage
			if req.Coverage == 0 {
				r.MustGaps = append(r.MustGaps, req.Text)
			}
		case Nice:
			r.NiceCount++
			niceSum += req.Coverage
			if req.Coverage == 0 {
				r.NiceGaps = append(r.NiceGaps, req.Text)
			}
		}
	}

	if r.MustCount > 0 {
		r.MustCoverage = mustSum / float64(r.MustCount)
	}
	if r.NiceCount > 0 {
		r.NiceCoverage = niceSum / float64(r.NiceCount)
	}

	switch {
	case r.MustCount == 0 && r.NiceCount == 0:
		r.Score = 0
	case r.NiceCount == 0:
		r.Score = 100 * r.MustCoverage
	case r.MustCount == 0:
		r.Score = 100 * r.NiceCoverage
	default:
		r.Score = 100 * (mustWeight*r.MustCoverage + niceWeight*r.NiceCoverage)
	}

	r.applyCaps(opts)
	return r
}

// applyCaps lowers the score to the strictest applicable cap and records
// why. A cap never raises a score.
//
// The missing-must cap is not made redundant by the split formula: one
// uncovered must out of ten still leaves must coverage at 0.9, which
// reaches 92 with good nice coverage.
func (r *Result) applyCaps(opts Options) {
	type cap struct {
		applies bool
		at      float64
		reason  string
	}
	for _, c := range []cap{
		{opts.BeyondCommute, capCommute, "onsite and beyond the commute limit"},
		{opts.LanguageShortfall, capLanguage, "language requirement above the inventory level"},
		{len(r.MustGaps) > 0, capMissingMust, fmt.Sprintf("%d must-have(s) not covered", len(r.MustGaps))},
	} {
		if c.applies && r.Score > c.at {
			r.Score = c.at
			r.CappedReason = c.reason
		}
	}
}
