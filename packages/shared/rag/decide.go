package rag

import (
	"errors"
	"fmt"
	"math"
)

// Floor is the minimum cosine similarity a result must reach to be answered
// from, together with whether that number was ever measured.
//
// Calibrated travels with the value everywhere the floor is visible — the
// answer payload, the gauges, the boot log and the README — because spec:315
// puts the measurement in P6 and a reader otherwise cannot tell a placeholder
// from a threshold.
type Floor struct {
	Value      float64
	Calibrated bool
}

// DefaultFloor is -1: the bottom of the cosine range, and so the one value
// that cannot be mistaken for a tuned threshold, because it excludes nothing.
// Spec:315 puts the number in P6, measured from the eval's distribution; a
// plausible default here would be a guess wearing a measurement's clothes.
func DefaultFloor() Floor { return Floor{Value: -1, Calibrated: false} }

// Validate fails closed like chunk.Options and walk.Limits: a floor outside
// the cosine range is a misconfiguration rather than a preference, and NaN is
// one that every comparison in Decide would silently accept.
func (f Floor) Validate() error {
	if math.IsNaN(f.Value) {
		return errors.New("rag: floor must be a number, got NaN")
	}
	if f.Value < -1 || f.Value > 1 {
		return fmt.Errorf("rag: floor must be within the cosine range [-1,1], got %v", f.Value)
	}
	return nil
}

// Outcome and Reason are metric labels as well as response fields, so both are
// closed sets: spec §10 requires a refusal and an error to be distinct
// outcomes, and a reason that can take any value cannot be a label at all.
type Outcome string

const (
	OutcomeAnswered Outcome = "answered"
	OutcomeRefused  Outcome = "refused"
)

type Reason string

const (
	ReasonNoSpans    Reason = "no_spans"
	ReasonBelowFloor Reason = "below_floor"
	ReasonUnscored   Reason = "unscored"
)

// Decide is the grounded-or-refused rule (spec §8), and the reason it returns
// is a metric label, so "we had nothing to say" never hides inside an error
// rate.
//
// top is the cosine similarity of the best-ranked span in the vector arm, not
// the fused score — see Fuse, and the test that shows a fused score is
// identical for a perfect arm and a worthless one. vectorRan is false in
// lexical-only mode, where there is no such number: ts_rank_cd is unbounded
// and corpus-dependent, so comparing it to a cosine floor would be a category
// error that happens to typecheck.
//
// The boundary is inclusive — a score equal to the floor answers. Arbitrary,
// but it has to be pinned somewhere or it drifts between code, tests and
// README.
func Decide(f Floor, hits int, top float64, vectorRan bool) (Outcome, Reason) {
	if hits == 0 {
		return OutcomeRefused, ReasonNoSpans
	}
	if !vectorRan {
		return OutcomeAnswered, ""
	}
	// Explicit, because NaN < f.Value is false and the natural spelling of the
	// floor check therefore answers confidently on a score that is not a
	// number. P2 measured pgvector returning NaN for a zero vector's cosine
	// distance.
	if math.IsNaN(top) {
		return OutcomeRefused, ReasonUnscored
	}
	if top < f.Value {
		return OutcomeRefused, ReasonBelowFloor
	}
	return OutcomeAnswered, ""
}
