package rag

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// fuseP3 is P3's arithmetic, reproduced here verbatim: each arm contributes
// 1/(k+rank) with an implicit weight of 1.0 and no multiplier anywhere.
//
// It exists so "the defaults changed nothing" is a byte-identical comparison
// against the old computation rather than against numbers copied out of a
// comment. 1.0 * x == x exactly in IEEE-754 for every finite x, so this is safe
// to claim rather than hope.
func fuseP3(k int, vector, lexical []Hit) []Fused {
	p := Params{K: k, WVector: 1, WLexical: 1}
	acc := map[string]*Fused{}
	entry := func(h Hit) *Fused {
		f, ok := acc[h.SpanID]
		if !ok {
			f = &Fused{SpanID: h.SpanID, Path: h.Path, StartLine: h.StartLine}
			acc[h.SpanID] = f
		}
		return f
	}
	for i, h := range vector {
		f := entry(h)
		f.VectorRank = i + 1
		f.VectorScore = h.Score
		f.Score += 1 / float64(k+i+1)
	}
	for i, h := range lexical {
		f := entry(h)
		f.LexicalRank = i + 1
		f.Score += 1 / float64(k+i+1)
	}
	_ = p
	out := make([]Fused, 0, len(acc))
	for _, f := range acc {
		out = append(out, *f)
	}
	sortFused(out)
	return out
}

func TestTheDefaultParamsFuseByteIdenticallyToP3(t *testing.T) {
	// P3's own fixtures, not fixtures written for this task: one written here
	// could be written to accommodate a new default.
	for _, tc := range []struct{ vector, lexical []Hit }{
		{hits("A", "B", "C", "D"), hits("C", "A", "E")},
		{hits("A", "B", "C"), nil},
		{nil, hits("C", "B", "A")},
		{hits("X", "Y"), hits("Y", "X")},
	} {
		got := Fuse(DefaultParams(), tc.vector, tc.lexical)
		want := fuseP3(60, tc.vector, tc.lexical)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Fuse(DefaultParams()) = %+v\nwant P3's %+v", got, want)
		}
	}
	// And the arithmetic P3's own comment records, to the bit: a span in both
	// arms at ranks 1 and 2 scores 1/61 + 1/62.
	//
	// Computed at RUNTIME, not as an untyped constant. Measured: `1.0/61 +
	// 1.0/62` written as a constant expression is 0.03252247488101533 because Go
	// evaluates constant arithmetic at arbitrary precision and rounds once,
	// while the running code rounds twice and gets 0.03252247488101534. The
	// difference is the constant folding, not the weights — the DeepEqual
	// against fuseP3 above is what actually pins "nothing changed".
	got := Fuse(DefaultParams(), hits("A", "B", "C", "D"), hits("C", "A", "E"))
	if want := 1/float64(61) + 1/float64(62); got[0].Score != want {
		t.Errorf("fused score %v, want %v (P3's value for a span in both arms at ranks 1 and 2)", got[0].Score, want)
	}
	if DefaultParams() != (Params{K: 60, WVector: 1, WLexical: 1}) {
		t.Errorf("DefaultParams() = %+v", DefaultParams())
	}
}

// Fixture rule 6: two arms ranking three documents in OPPOSITE orders, with the
// crossover computed in the test body from k and the ranks rather than written
// as a literal. A literal crossover stops being the crossover the day k's
// default changes.
func TestWeightsOnlyMatterWhereTheArmsDisagree(t *testing.T) {
	vector, lexical := hits("X", "Y", "Z"), hits("Z", "Y", "X")

	// Build the disagreement, then ASSERT it: a property a kill depends on has
	// to fail loudly when it stops holding.
	if vector[0].SpanID == lexical[0].SpanID {
		t.Fatalf("the arms agree at rank 1, so no weight can change the order")
	}

	k := DefaultParams().K
	// X is vector rank 1 and lexical rank 3; Z is the mirror. So
	//   score(X) - score(Z) = (wv - wl) * (rrf(k,1) - rrf(k,3))
	// and the crossover is at wv == wl, whatever k is.
	spread := rrf(k, 1) - rrf(k, 3)
	if spread <= 0 {
		t.Fatalf("rrf is not decreasing in rank at k=%d; the crossover derivation does not hold", k)
	}
	for _, tc := range []struct {
		wv, wl float64
		first  string
	}{
		{2, 1, "X"},
		{1, 2, "Z"},
		{1, 1, "X"}, // the mirror pair ties; the tie-break decides, not the weights
	} {
		p := Params{K: k, WVector: tc.wv, WLexical: tc.wl}
		got := Fuse(p, vector, lexical)
		if got[0].SpanID != tc.first {
			t.Errorf("wv=%v wl=%v: rank 1 is span %q, want %q", tc.wv, tc.wl, got[0].SpanID, tc.first)
		}
		// The derivation, asserted rather than assumed: X is vector rank 1 and
		// lexical rank 3, Z is the mirror, so their difference is exactly
		// (wv - wl) * spread. Y is rank 2 in BOTH arms and is not part of the
		// pair — an earlier draft of this test asserted every span ties at
		// wv == wl and was wrong for that reason.
		byID := map[string]Fused{}
		for _, f := range got {
			byID[f.SpanID] = f
		}
		//
		// Compared to a relative tolerance, not bit-exactly: the two sides sum
		// the same terms in different orders and differ in the last bit
		// (measured: 0.0005204267499349449 against 0.0005204267499349484). The
		// SIGN is what decides the ranking and it is compared exactly.
		got_, want := byID["X"].Score-byID["Z"].Score, (tc.wv-tc.wl)*spread
		if (got_ > 0) != (want > 0) || (got_ < 0) != (want < 0) {
			t.Errorf("wv=%v wl=%v: score(X)-score(Z) = %v, want the sign of %v", tc.wv, tc.wl, got_, want)
		}
		if math.Abs(got_-want) > 1e-12*math.Max(1, math.Abs(want)) {
			t.Errorf("wv=%v wl=%v: score(X)-score(Z) = %v, want %v", tc.wv, tc.wl, got_, want)
		}
	}

	// The weight scales the arm's CONTRIBUTION, not the rank discount. A span
	// only in the vector arm at rank 1 scores exactly wv/(k+1); under
	// 1/(wv*k+rank) it would score 1/(2*60+1).
	only := Fuse(Params{K: k, WVector: 2, WLexical: 1}, hits("solo"), nil)
	if want := 2.0 / float64(k+1); only[0].Score != want {
		t.Errorf("fused score for a rank-1 vector hit is %v, want %v", only[0].Score, want)
	}
	// And at depth, where the two forms diverge further.
	deep := Fuse(Params{K: k, WVector: 2, WLexical: 1}, hits("a", "b", "c", "d", "solo"), nil)
	if want := 2.0 / float64(k+5); deep[len(deep)-1].Score != want {
		t.Errorf("fused score for a rank-5 vector hit is %v, want %v", deep[len(deep)-1].Score, want)
	}
}

func TestAWeightOfZeroRemovesAnArmFromTheRankingAndNotFromTheResult(t *testing.T) {
	vector, lexical := hits("X", "Y"), hits("Z")
	got := Fuse(Params{K: 60, WVector: 0, WLexical: 1}, vector, lexical)

	// Z, the only lexical hit, now outranks both vector hits.
	if got[0].SpanID != "Z" {
		t.Errorf("rank 1 is %q, want Z: the vector arm should contribute nothing to the ranking", got[0].SpanID)
	}
	// But the vector arm's spans are still in the result, and still carry their
	// rank and their cosine similarity — which is what the floor reads.
	byID := map[string]Fused{}
	for _, f := range got {
		byID[f.SpanID] = f
	}
	if len(got) != 3 {
		t.Errorf("fused %d spans, want 3: a zero weight removes an arm from the RANKING, not from the RESULT", len(got))
	}
	if byID["X"].VectorRank != 1 || byID["X"].VectorScore != vector[0].Score {
		t.Errorf("X = %+v, want its vector rank and cosine intact", byID["X"])
	}
	if byID["X"].Score != 0 {
		t.Errorf("X's fused score is %v, want 0", byID["X"].Score)
	}
}

func TestANegativeWeightIsRefusedByValidate(t *testing.T) {
	for name, p := range map[string]Params{
		"negative vector":  {K: 60, WVector: -1, WLexical: 1},
		"negative lexical": {K: 60, WVector: 1, WLexical: -1},
		"NaN vector":       {K: 60, WVector: math.NaN(), WLexical: 1},
		"negative k":       {K: -1, WVector: 1, WLexical: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", p)
			}
		})
	}

	// And the ranking, so a mutant that validates and then ignores the value
	// also fails: with WLexical = -1 the worst lexical hit would outrank the
	// best one, silently.
	r, _, _ := retriever(t, ModeHybrid)
	r.Fusion.WLexical = -1
	res, err := r.Search(context.Background(), "repo-1", "q", 10)
	if err == nil {
		t.Errorf("Search returned rank 1 %q with WLexical -1, want an error naming the weight", ranked(res)[0])
	} else if !strings.Contains(err.Error(), "w_lexical") {
		t.Errorf("the error does not name the weight: %v", err)
	}
}

func TestBothWeightsZeroIsRefusedByValidate(t *testing.T) {
	if err := (Params{K: 60}).Validate(); err == nil {
		t.Fatalf("Validate accepted both weights at zero")
	}
	// The zero-value hazard: a Retriever built without touching Fusion. The
	// fixture's expected order disagrees with path order, so the degenerate
	// ranking and the correct one do not look the same.
	r, _, _ := retriever(t, ModeHybrid)
	r.Fusion = Params{}
	res, err := r.Search(context.Background(), "repo-1", "q", 10)
	if err == nil {
		t.Errorf("Search answered %d hits in %v with no error, want an error naming the weights", len(res.Hits), ranked(res))
	} else if !strings.Contains(err.Error(), "w_vector") || !strings.Contains(err.Error(), "w_lexical") {
		t.Errorf("the error does not name both weights: %v", err)
	}
}

// The idiom P6 will look for, documented where it is used.
func TestASweepIsAStructCopyAndDoesNotMutateTheOriginal(t *testing.T) {
	r1, _, _ := retriever(t, ModeHybrid)
	before := r1.Fusion

	r2 := *r1
	r2.Fusion.WLexical = 0.5
	if _, err := r2.Search(context.Background(), "repo-1", "q", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Search(context.Background(), "repo-1", "q", 10); err != nil {
		t.Fatal(err)
	}
	// A third search on the original, after the copy has run, because a Search
	// that wrote back into its receiver would make point n depend on point n-1
	// and the resulting curve an artefact of iteration order.
	res, err := r1.Search(context.Background(), "repo-1", "q", 10)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Fusion != before {
		t.Errorf("the original's Fusion is %+v, want %+v", r1.Fusion, before)
	}
	if r1.Fusion.WLexical != 1 {
		t.Errorf("the original's Fusion.WLexical is %v, want 1", r1.Fusion.WLexical)
	}
	if want := []string{"A", "C", "B", "E", "D"}; !reflect.DeepEqual(ranked(res), want) {
		t.Errorf("the original ranked %v after the sweep, want %v", ranked(res), want)
	}
}

// The floor reads the VECTOR ARM's cosine similarity, never the fused score. A
// per-arm weight changes the fused score, and Decide does not read it.
//
// THE FIXTURE HAS TO MAKE THE FUSED TOP SCORE EXCEED THE FLOOR, or a mutant
// reading Hits[0].Score refuses too — for the wrong reason, which reads as a
// kill and is not one. The plan's fixture (floor 0.5, WVector=3) does NOT do
// this: at k=60 the fused top is 3/61 = 0.049, still far under 0.5, and the
// mutant refuses. Measured — the mutation survived this test on the first run
// and was killed only by P6's Result.Decide test.
//
// k=0 is what makes it discriminate: the rank-1 contribution is then wv/(0+1),
// so the fused top is at least 1.0 and a mutant reading it answers where the
// original refuses.
func TestTheFloorReadsTheVectorArmWhateverTheWeightsAre(t *testing.T) {
	floor := Floor{Value: 0.5}
	for _, p := range []Params{
		{K: 0, WVector: 1, WLexical: 1},
		{K: 0, WVector: 3, WLexical: 1},
		{K: 0, WVector: 0, WLexical: 1},
		{K: 60, WVector: 1, WLexical: 1},
	} {
		r, st, _ := retriever(t, ModeHybrid)
		r.Fusion = p
		// A top cosine of 0.2, well under the floor.
		st.vector = []models.Cite{{Span: models.Span{ID: "A", Path: "A.go"}, Score: 0.2}}
		res, err := r.Search(context.Background(), "repo-1", "q", 10)
		if err != nil {
			t.Fatal(err)
		}
		// float32 on the row, so 0.2 widens to 0.20000000298023224.
		if res.TopScore != float64(float32(0.2)) {
			t.Fatalf("%+v: the fixture's vector top score is %v, want 0.2", p, res.TopScore)
		}
		outcome, reason := res.Decide(floor)
		if outcome != OutcomeRefused || reason != ReasonBelowFloor {
			t.Errorf("%+v: answered with top_score %v (fused %v), want refused below_floor with top_score 0.2",
				p, res.TopScore, res.Hits[0].Score)
		}
	}

	// And the fixture is proved able to discriminate: at k=0 the fused top score
	// really does clear the floor, so a Decide reading it would answer.
	r, st, _ := retriever(t, ModeHybrid)
	r.Fusion = Params{K: 0, WVector: 1, WLexical: 1}
	st.vector = []models.Cite{{Span: models.Span{ID: "A", Path: "A.go"}, Score: 0.2}}
	res, _ := r.Search(context.Background(), "repo-1", "q", 10)
	if res.Hits[0].Score <= floor.Value {
		t.Fatalf("the fused top score is %v, under the floor of %v: a Decide reading it would refuse too and this test could not discriminate",
			res.Hits[0].Score, floor.Value)
	}
}

// The Fuse CALL SITE, not Fuse itself.
//
// Every other weight test drives Fuse directly, so a Retriever that handed it
// WLexical where WVector belongs would be invisible to all of them — and at the
// shipped 1.0/1.0 the swap is a semantic no-op, so the fixture has to set
// unequal weights AND go through Search. Found by mutating: the swap survived
// the whole of this task's first round.
//
// The fixture's arms disagree ([A B C D] against [C A E]), so at WVector=2 the
// order is [A C B D E] and under a swap it is [C A E B D] — rank 1 itself
// changes, and E moves from last to third.
func TestTheRetrieversWeightsReachFuseInTheOrderTheyWereWritten(t *testing.T) {
	for _, tc := range []struct {
		wv, wl float64
		want   []string
	}{
		{2, 1, []string{"A", "C", "B", "D", "E"}},
		{1, 2, []string{"C", "A", "E", "B", "D"}},
	} {
		r, _, _ := retriever(t, ModeHybrid)
		r.Fusion.WVector, r.Fusion.WLexical = tc.wv, tc.wl
		res, err := r.Search(context.Background(), "repo-1", "q", 10)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ranked(res), tc.want) {
			t.Errorf("wv=%v wl=%v ranked %v, want %v", tc.wv, tc.wl, ranked(res), tc.want)
		}
	}
	// And the two orders differ, so the assertion above can discriminate.
	a, _, _ := retriever(t, ModeHybrid)
	a.Fusion.WVector = 2
	b, _, _ := retriever(t, ModeHybrid)
	b.Fusion.WLexical = 2
	ra, _ := a.Search(context.Background(), "repo-1", "q", 10)
	rb, _ := b.Search(context.Background(), "repo-1", "q", 10)
	if reflect.DeepEqual(ranked(ra), ranked(rb)) {
		t.Fatalf("the two weightings rank identically (%v), so a swap is undetectable", ranked(ra))
	}
}
