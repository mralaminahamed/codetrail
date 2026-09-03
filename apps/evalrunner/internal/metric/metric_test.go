package metric

import (
	"math"
	"reflect"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// The canonical fixture, and why every number in it is chosen.
//
//	case A: ranked [s1 s2 s3 s4 s5], gold {s1}      -> RR = 1/1 = 1
//	case B: ranked [s1 s2 s3 s4 s5], gold {s3, s5}  -> RR = 1/3, not 1/5
//	case C: ranked [s1 s2 s3 s4 s5], gold {s9}      -> RR = 0
//	MRR = (1 + 1/3 + 0)/3 = 4/9
//
// Case B has two gold spans at different ranks, which is the window arm's real
// shape and the only thing that separates "first gold hit" from "last". Case C
// is the miss that puts a zero in the numerator and a one in the denominator.
// Under the three mutations the value is 2/3, 1/4 and 2/5 — all inside [0,1],
// which is why every assertion below is a value and none is a range.
var ranked = []string{"s1", "s2", "s3", "s4", "s5"}

var canonical = []struct {
	name string
	gold map[string]bool
	rr   float64
}{
	{"A", Set([]string{"s1"}), 1},
	{"B", Set([]string{"s3", "s5"}), 1.0 / 3.0},
	{"C", Set([]string{"s9"}), 0},
}

func TestReciprocalRankIsOneOverTheRankOfTheFirstGoldHit(t *testing.T) {
	if got := ReciprocalRank(ranked, canonical[0].gold); got != 1 {
		t.Errorf("case A reciprocal rank %v, want 1", got)
	}
	if got := mrr(); got != 4.0/9.0 {
		t.Errorf("MRR %v, want %v", got, 4.0/9.0)
	}
}

func TestReciprocalRankIsZeroWhenNoGoldSpanIsRanked(t *testing.T) {
	if got := ReciprocalRank(ranked, canonical[2].gold); got != 0 {
		t.Errorf("case C reciprocal rank %v, want 0", got)
	}
	if got := ReciprocalRank(nil, canonical[0].gold); got != 0 {
		t.Errorf("an empty ranked list gave %v, want 0", got)
	}
}

func TestReciprocalRankUsesTheFirstGoldHitAndNotTheLast(t *testing.T) {
	got := ReciprocalRank(ranked, canonical[1].gold)
	if got != 1.0/3.0 {
		t.Errorf("case B reciprocal rank %v, want %v (s3 at rank 3, not s5 at rank 5)", got, 1.0/3.0)
	}
	if got == 0.2 {
		t.Errorf("case B reciprocal rank is 1/5: the last gold hit, not the first")
	}
	if m := mrr(); m != 4.0/9.0 {
		t.Errorf("MRR %v, want %v", m, 4.0/9.0)
	}
}

func TestMeanOverAllCountsTheMissesInTheDenominator(t *testing.T) {
	if got := mrr(); got != 4.0/9.0 {
		t.Errorf("MRR is %v, want %v", got, 4.0/9.0)
	}
	// The mutant that drops the zeros reports this instead, and it is inside
	// [0,1] like every other wrong answer here.
	if got := mrr(); got == 2.0/3.0 {
		t.Errorf("MRR is 2/3: the mean over the cases that hit, not over the golden set")
	}
	if got := MeanOverAll([]float64{0, 0, 0}); got != 0 {
		t.Errorf("the mean of three misses is %v, want 0", got)
	}
	if got := MeanOverAll(nil); got != 0 {
		t.Errorf("the mean of nothing is %v, want 0", got)
	}
}

func TestHitAtKIncludesRankExactlyK(t *testing.T) {
	gold := Set([]string{"s3"})
	for _, c := range []struct {
		k    int
		want bool
	}{{1, false}, {2, false}, {3, true}, {5, true}} {
		if got := HitAtK(ranked, gold, c.k); got != c.want {
			t.Errorf("hit@%d is %v for a gold span at rank 3, want %v", c.k, got, c.want)
		}
	}
	if HitAtK(ranked, canonical[2].gold, 5) {
		t.Error("hit@5 is true for a gold span that is not ranked at all")
	}
}

func mrr() float64 {
	rr := make([]float64, 0, len(canonical))
	for _, c := range canonical {
		rr = append(rr, ReciprocalRank(ranked, c.gold))
	}
	return MeanOverAll(rr)
}

// The three outcomes an ordinary hybrid case never produces, and the three an
// inline `TopScore < f` comparison gets wrong in the same direction. All three
// have a NaN top score, and NaN < f is false, so all three would be answered
// at every floor.
var delegation = []Outcome{
	{CaseID: "ordinary", Hits: 5, TopScore: 0.72, VectorRan: true, AnswerHasGold: true, Scoreable: true},
	{CaseID: "no_spans", Hits: 0, TopScore: math.NaN(), VectorRan: true, AnswerHasGold: false, Scoreable: true},
	{CaseID: "unscored", Hits: 3, TopScore: math.NaN(), VectorRan: true, AnswerHasGold: false, Scoreable: true},
	{CaseID: "lexical", Hits: 4, TopScore: math.NaN(), VectorRan: false, AnswerHasGold: true, Scoreable: true},
}

func TestSweepDelegatesToDecideAndNotToAComparison(t *testing.T) {
	got := Sweep(delegation, []float64{0.5})[0]
	want := Confusion{
		Floor: 0.5, AnsweredWithGold: 2, AnsweredWithoutGold: 0,
		RefusedWithGold: 0, RefusedWithoutGold: 2, Unscoreable: 0,
		ByReason: map[rag.Reason]int{rag.ReasonNoSpans: 1, rag.ReasonUnscored: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("confusion at floor 0.5 is %+v, want %+v", got, want)
	}
	// Named, because the whole point is which cases move: an inline
	// comparison answers all four and reports no reason at all.
	if len(got.ByReason) == 0 {
		t.Error("no refusal carried a reason; no_spans and unscored are different facts about a corpus and a floor sweep that cannot tell them apart cannot say what the floor is doing")
	}
}

// Every outcome here is hybrid, scoreable and has hits, so the only thing the
// sweep can be wrong about is the floor comparison. Two of them score exactly
// 0.5, which is the swept floor: an arbitrary fixture never lands on a
// boundary and cannot discriminate.
var boundary = []Outcome{
	{CaseID: "eq-gold", Hits: 5, TopScore: 0.5, VectorRan: true, AnswerHasGold: true, Scoreable: true},
	{CaseID: "eq-nogold", Hits: 5, TopScore: 0.5, VectorRan: true, AnswerHasGold: false, Scoreable: true},
	{CaseID: "under-gold", Hits: 5, TopScore: 0.3, VectorRan: true, AnswerHasGold: true, Scoreable: true},
	{CaseID: "over-nogold", Hits: 5, TopScore: 0.7, VectorRan: true, AnswerHasGold: false, Scoreable: true},
}

func TestSweepIsInclusiveAtTheFloor(t *testing.T) {
	got := Sweep(boundary, []float64{0.5})[0]
	want := Confusion{
		Floor: 0.5, AnsweredWithGold: 1, AnsweredWithoutGold: 2,
		RefusedWithGold: 1, RefusedWithoutGold: 0, Unscoreable: 0,
		ByReason: map[rag.Reason]int{rag.ReasonBelowFloor: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("at floor 0.5 the confusion is %+v, want %+v; a score equal to the floor answers", got, want)
	}
	// The floor is swept, so the curve has to move in the right direction too.
	sweep := Sweep(boundary, []float64{0.4, 0.5, 0.6})
	for i, c := range sweep {
		if c.Floor != []float64{0.4, 0.5, 0.6}[i] {
			t.Errorf("sweep[%d] is at floor %v", i, c.Floor)
		}
	}
	if sweep[0].RefusedWithGold != 1 || sweep[2].RefusedWithGold != 2 {
		t.Errorf("refused-with-gold across 0.4/0.5/0.6 is %d/%d/%d, want 1/1/2",
			sweep[0].RefusedWithGold, sweep[1].RefusedWithGold, sweep[2].RefusedWithGold)
	}
}

func TestSweepKeepsUnscoreableCasesOutOfEveryDenominator(t *testing.T) {
	outcomes := append(append([]Outcome{}, boundary...),
		Outcome{CaseID: "no-gold-in-this-arm", Hits: 5, TopScore: 0.9, VectorRan: true, AnswerHasGold: false, Scoreable: false})
	got := Sweep(outcomes, []float64{0.5})[0]
	want := Confusion{
		Floor: 0.5, AnsweredWithGold: 1, AnsweredWithoutGold: 2,
		RefusedWithGold: 1, RefusedWithoutGold: 0, Unscoreable: 1,
		ByReason: map[rag.Reason]int{rag.ReasonBelowFloor: 1},
	}
	// The whole struct, not one cell: a mutant that moves a case from one cell
	// to another leaves any single-cell assertion passing.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("confusion at floor 0.5 is %+v, want %+v", got, want)
	}
	if sum := got.AnsweredWithGold + got.AnsweredWithoutGold + got.RefusedWithGold + got.RefusedWithoutGold; sum != 4 {
		t.Errorf("the four scored cells hold %d cases, want 4: the unscoreable case has no right answer in this arm", sum)
	}
}

func TestSweepNamesBothDirectionsOfRefusal(t *testing.T) {
	// A floor low enough to answer everything, and one high enough to refuse
	// everything, so both directions are exercised against the same cases.
	sweep := Sweep(boundary, []float64{-1, 1})
	if sweep[0].RefusedWithGold != 0 || sweep[0].RefusedWithoutGold != 0 {
		t.Errorf("at floor -1 the confusion is %+v; nothing can fall under the bottom of the cosine range", sweep[0])
	}
	if sweep[0].AnsweredWithGold != 2 || sweep[0].AnsweredWithoutGold != 2 {
		t.Errorf("at floor -1 the answered cells are %d/%d, want 2/2", sweep[0].AnsweredWithGold, sweep[0].AnsweredWithoutGold)
	}
	if sweep[1].RefusedWithGold != 2 || sweep[1].RefusedWithoutGold != 2 {
		t.Errorf("at floor 1 the refused cells are %d/%d, want 2/2", sweep[1].RefusedWithGold, sweep[1].RefusedWithoutGold)
	}
	if sweep[1].AnsweredWithGold != 0 || sweep[1].AnsweredWithoutGold != 0 {
		t.Errorf("at floor 1 the answered cells are %d/%d, want 0/0", sweep[1].AnsweredWithGold, sweep[1].AnsweredWithoutGold)
	}
}

func TestAFloorSweepIsInapplicableInLexicalMode(t *testing.T) {
	for _, c := range []struct {
		mode rag.Mode
		want bool
	}{{rag.ModeHybrid, true}, {rag.ModeVector, true}, {rag.ModeLexical, false}} {
		if got := Applicable(c.mode); got != c.want {
			t.Errorf("Applicable(%q) is %v, want %v", c.mode, got, c.want)
		}
	}
	// And the reason: Decide short-circuits before it reads the floor, so the
	// same lexical outcome lands in the same cell at every floor.
	lexical := []Outcome{{CaseID: "l", Hits: 4, TopScore: math.NaN(), VectorRan: false, AnswerHasGold: true, Scoreable: true}}
	for _, c := range Sweep(lexical, []float64{-1, 0, 0.5, 1}) {
		if c.AnsweredWithGold != 1 || len(c.ByReason) != 0 {
			t.Errorf("at floor %v the lexical case gave %+v, want it answered at every floor", c.Floor, c)
		}
	}
}
