package rag

import (
	"math"
	"testing"
)

// Nothing retrieved is a refusal, not an answer over an empty citation list —
// and not an error (spec §10). It refuses at the shipped default too, which is
// what keeps the refusal path live before P6 measures a floor.
func TestNothingRetrievedRefusesEvenAtTheDefaultFloor(t *testing.T) {
	out, why := Decide(DefaultFloor(), 0, math.NaN(), true)
	if out != OutcomeRefused || why != ReasonNoSpans {
		t.Fatalf("got %s/%s, want refused/no_spans", out, why)
	}
}

// The boundary has to be pinned somewhere: equal to the floor answers.
func TestTheFloorIsInclusive(t *testing.T) {
	f := Floor{Value: 0.5}
	if out, why := Decide(f, 3, 0.5, true); out != OutcomeAnswered {
		t.Fatalf("a top score of exactly 0.5 gave %s/%s, want answered", out, why)
	}
	if out, why := Decide(f, 3, 0.49999999, true); out != OutcomeRefused || why != ReasonBelowFloor {
		t.Fatalf("just under the floor gave %s/%s, want refused/below_floor", out, why)
	}
}

// NaN < floor is false, so the obvious spelling of the check answers on a score
// that is not a number. pgvector returns NaN for the cosine distance of a zero
// vector (measured in P2), and a confident answer is the worst possible
// response to that.
func TestANaNTopScoreRefuses(t *testing.T) {
	out, why := Decide(Floor{Value: -1}, 3, math.NaN(), true)
	if out != OutcomeRefused || why != ReasonUnscored {
		t.Fatalf("got %s/%s, want refused/unscored", out, why)
	}
}

// The floor is defined on cosine similarity. In lexical-only mode there is no
// cosine similarity, and ts_rank_cd is unbounded, so the floor does not apply
// and only an empty result can refuse.
func TestTheFloorDoesNotApplyWhenTheVectorArmDidNotRun(t *testing.T) {
	if out, _ := Decide(Floor{Value: 0.9}, 3, math.NaN(), false); out != OutcomeAnswered {
		t.Fatalf("a lexical-only result was refused by a cosine floor")
	}
}

// The shipped default is not a threshold: -1 is the bottom of the cosine range
// and cannot exclude anything. If this test starts failing because the default
// moved, the README's "not calibrated" claim moved with it.
func TestTheShippedDefaultRefusesNothingOnScoreAlone(t *testing.T) {
	f := DefaultFloor()
	if f.Calibrated {
		t.Fatal("the default floor claims to be calibrated")
	}
	if out, why := Decide(f, 1, -1, true); out != OutcomeAnswered {
		t.Fatalf("the worst possible cosine gave %s/%s at the default floor", out, why)
	}
}

// Fail closed like chunk.Options and walk.Limits: a floor outside the cosine
// range is a misconfiguration, not a preference, and NaN is one a comparison
// would silently accept.
func TestFloorValidateRefusesWhatCosineCannotProduce(t *testing.T) {
	for _, v := range []float64{1.0001, -1.0001, math.NaN(), math.Inf(1)} {
		if err := (Floor{Value: v}).Validate(); err == nil {
			t.Fatalf("floor %v was accepted", v)
		}
	}
	for _, v := range []float64{-1, 0, 0.42, 1} {
		if err := (Floor{Value: v}).Validate(); err != nil {
			t.Fatalf("floor %v: %v", v, err)
		}
	}
}
