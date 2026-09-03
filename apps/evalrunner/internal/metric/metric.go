// Package metric is the arithmetic the eval reports: hit@k, reciprocal rank,
// their mean, and the four-cell refusal confusion across a floor sweep.
//
// Nothing here retrieves. The sweep runs over outcomes a retrieval pass
// already recorded, so the whole floor curve comes from one pass and is
// recomputable from the committed artefact with no embedder in the loop —
// which is what spec:249's "recorded with the numbers that produced it" has to
// mean if it is to mean anything.
package metric

import (
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// HitAtK reports whether a gold span is in the first k of ranked.
//
// Rank is 1-based and the boundary is inclusive: hit@5 means the answer is in
// the first five. Off by one here shifts every reported hit rate by exactly
// the mass at rank k, which for a good retriever is not small.
func HitAtK(ranked []string, gold map[string]bool, k int) bool {
	for i, id := range ranked {
		if i+1 > k {
			return false
		}
		if gold[id] {
			return true
		}
	}
	return false
}

// ReciprocalRank is 1/rank of the *first* gold hit, and 0 when there is none.
//
// Zero, not "excluded". MRR is defined over every case in the golden set, and
// averaging only the cases that hit reports a different, higher, and wrong
// number that no range assertion catches.
func ReciprocalRank(ranked []string, gold map[string]bool) float64 {
	for i, id := range ranked {
		if gold[id] {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// MeanOverAll is the mean of xs, misses included.
//
// A named function rather than an inline division because it is the failure
// that is one line long and invisible in a plot: dropping the zeros reports
// the mean reciprocal rank of the cases that worked.
func MeanOverAll(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// Outcome is one case's retrieval, recorded.
//
// Hits and VectorRan are here because Sweep calls rag.Decide, which reads four
// inputs. An Outcome carrying only TopScore would make the sweep a third
// derivation of the refusal rule and would count every no_spans, every
// unscored and every lexical case as answered at every floor — all of them
// into AnsweredWithoutGold, which is the cell the calibration criterion
// balances against.
//
// AnswerHasGold is about the assembled answer, not the ranked list: rag.Assemble
// drops spans past the budget, so a gold span ranked seventh is retrieved and
// not cited, and "answered without citing the right span" is what a user
// experiences.
type Outcome struct {
	CaseID        string  `json:"case_id"`
	TopScore      float64 `json:"top_score"`
	Hits          int     `json:"hits"`
	VectorRan     bool    `json:"vector_ran"`
	GoldRank      int     `json:"gold_rank"`
	AnswerHasGold bool    `json:"answer_has_gold"`
	// Scoreable is false for a case with no gold span in this arm. It cannot
	// be right or wrong here, so it is counted in its own cell and left out of
	// every denominator. The honest expectation is zero and a non-zero is a
	// finding about the chunker.
	Scoreable bool `json:"scoreable"`
}

// Confusion is the refusal decision at one floor, in both directions.
//
// RefusedWithGold is the system refusing when it should have answered;
// AnsweredWithoutGold is the system answering when it should have refused.
// Reporting one without the other is how a floor gets tuned to zero or to one.
type Confusion struct {
	Floor               float64            `json:"floor"`
	AnsweredWithGold    int                `json:"answered_with_gold"`
	AnsweredWithoutGold int                `json:"answered_without_gold"`
	RefusedWithGold     int                `json:"refused_with_gold"`
	RefusedWithoutGold  int                `json:"refused_without_gold"`
	Unscoreable         int                `json:"unscoreable"`
	ByReason            map[rag.Reason]int `json:"by_reason"`
}

// Sweep tallies rag.Decide's verdict on every outcome, at each floor.
//
// It calls Decide rather than spelling the comparison. The inline spelling is
// correct for the ordinary case and wrong for exactly the three branches it
// cannot see — a no_spans case, an unscored case and a lexical-mode case all
// have a NaN top score, `NaN < f` is false, and all three would be answered at
// every floor. A P6 that reimplements the refusal rule has found a P3 defect,
// not a P6 requirement.
func Sweep(outcomes []Outcome, floors []float64) []Confusion {
	out := make([]Confusion, 0, len(floors))
	for _, f := range floors {
		c := Confusion{Floor: f, ByReason: map[rag.Reason]int{}}
		for _, o := range outcomes {
			if !o.Scoreable {
				c.Unscoreable++
				continue
			}
			outcome, reason := rag.Decide(rag.Floor{Value: f}, o.Hits, o.TopScore, o.VectorRan)
			if outcome == rag.OutcomeRefused {
				c.ByReason[reason]++
				if o.AnswerHasGold {
					c.RefusedWithGold++
				} else {
					c.RefusedWithoutGold++
				}
				continue
			}
			if o.AnswerHasGold {
				c.AnsweredWithGold++
			} else {
				c.AnsweredWithoutGold++
			}
		}
		out = append(out, c)
	}
	return out
}

// Applicable reports whether a floor sweep says anything in this mode.
//
// In lexical mode Decide short-circuits to answered whatever the score, so a
// sweep there is a sweep over a threshold nothing reads. "The floor is
// inapplicable" is a different sentence from "the floor refused nothing", and
// the artefact has to say which.
func Applicable(m rag.Mode) bool { return m != rag.ModeLexical }
