package scoring

import "testing"

func reqs(kind Kind, coverages ...float64) []Requirement {
	var out []Requirement
	for i, c := range coverages {
		out = append(out, Requirement{
			Text: string(kind) + string(rune('A'+i)), Kind: kind, Coverage: c,
			IDs: []string{"id"},
		})
	}
	return out
}

func near(a, b float64) bool { return a-b < 0.05 && b-a < 0.05 }

func TestMixesMustAndNiceAtTheFixedRatio(t *testing.T) {
	got := Compute(append(reqs(Must, 1, 1, 1), reqs(Nice, 1, 0)...), Options{})
	// must 1.0, nice 0.5 -> 80 + 10
	if !near(got.Score, 90) {
		t.Errorf("got %.1f, want 90", got.Score)
	}
}

// The property the split formula exists for: a long advertisement and a
// short one with the same must coverage must land close together.
func TestVerbosityDoesNotMoveTheScore(t *testing.T) {
	terse := Compute(append(reqs(Must, 1, 1, 1), reqs(Nice, 0)...), Options{})

	verbose := Compute(append(reqs(Must, 1, 1, 1),
		reqs(Nice, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)...), Options{})

	if d := verbose.Score - terse.Score; d > 5 || d < -5 {
		t.Errorf("terse %.1f vs verbose %.1f: spread of %.1f is too wide",
			terse.Score, verbose.Score, d)
	}
}

func TestAdWithOnlyMustRequirementsUsesMustCoverageAlone(t *testing.T) {
	got := Compute(reqs(Must, 1, 1, 1, 1), Options{})
	if !near(got.Score, 100) {
		t.Errorf("got %.1f, want 100", got.Score)
	}
}

func TestAdWithOnlyNiceRequirementsUsesNiceCoverageAlone(t *testing.T) {
	got := Compute(reqs(Nice, 1, 0), Options{})
	if !near(got.Score, 50) {
		t.Errorf("got %.1f, want 50", got.Score)
	}
}

func TestEmptyRequirementListScoresZero(t *testing.T) {
	if got := Compute(nil, Options{}); got.Score != 0 {
		t.Errorf("got %.1f, want 0", got.Score)
	}
}

// A missing must-have must never reach the letter threshold, however good
// everything else looks.
func TestUncoveredMustIsCappedBelowTheThreshold(t *testing.T) {
	got := Compute(append(
		reqs(Must, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0),
		reqs(Nice, 1, 1, 1)...), Options{})

	if got.Score > capMissingMust {
		t.Errorf("got %.1f, want at most %.0f", got.Score, capMissingMust)
	}
	if got.Score >= 80 {
		t.Errorf("score %.1f would trigger a letter despite a must-gap", got.Score)
	}
	if len(got.MustGaps) != 1 {
		t.Errorf("got %d must gaps, want 1", len(got.MustGaps))
	}
	if got.CappedReason == "" {
		t.Error("the cap was applied without recording a reason")
	}
}

func TestStrictestCapWins(t *testing.T) {
	all := Compute(reqs(Must, 1, 1), Options{LanguageShortfall: true, BeyondCommute: true})
	if !near(all.Score, capCommute) {
		t.Errorf("got %.1f, want the strictest cap %.0f", all.Score, capCommute)
	}
}

func TestCapNeverRaisesAScore(t *testing.T) {
	got := Compute(reqs(Must, 0.2, 0.2), Options{LanguageShortfall: true})
	if got.Score > 20.01 {
		t.Errorf("cap raised the score to %.1f", got.Score)
	}
}

func TestIdenticalInputGivesAnIdenticalScore(t *testing.T) {
	in := append(reqs(Must, 1, 0.8), reqs(Nice, 0.4)...)
	if Compute(in, Options{}).Score != Compute(in, Options{}).Score {
		t.Error("the same input produced two different scores")
	}
}

func TestGapsAreListedSeparatelyByKind(t *testing.T) {
	got := Compute(append(reqs(Must, 1, 0), reqs(Nice, 0, 0)...), Options{})
	if len(got.MustGaps) != 1 || len(got.NiceGaps) != 2 {
		t.Errorf("must gaps %v, nice gaps %v", got.MustGaps, got.NiceGaps)
	}
}
