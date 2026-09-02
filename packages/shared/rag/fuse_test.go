package rag

import "testing"

// hits builds an arm's ranked list. Scores descend so the list is already in
// the order the arm would return it; Fuse must not re-sort by score.
func hits(ids ...string) []Hit {
	out := make([]Hit, len(ids))
	for i, id := range ids {
		out[i] = Hit{SpanID: id, Path: id + ".go", StartLine: 1, Score: 1 - float64(i)/100}
	}
	return out
}

func ids(fs []Fused) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.SpanID
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The fixture is built so the fused order matches neither arm's own order and
// neither alphabetical nor insertion order: vector is [A B C D], lexical is
// [C A E], and the answer is [A C B E D]. A Fuse that returns one arm verbatim,
// or that sorts by id, or that keeps insertion order, fails here.
//
// k=60: A=1/61+1/62=0.0325224, C=1/63+1/61=0.0322664, B=1/62=0.0161290,
// E=1/63=0.0158730, D=1/64=0.0156250.
func TestFuseIsReciprocalRankOverBothArms(t *testing.T) {
	got := Fuse(60, hits("A", "B", "C", "D"), hits("C", "A", "E"))
	eq(t, ids(got), []string{"A", "C", "B", "E", "D"})
	if got[0].VectorRank != 1 || got[0].LexicalRank != 2 {
		t.Fatalf("A has ranks v=%d l=%d, want v=1 l=2", got[0].VectorRank, got[0].LexicalRank)
	}
	// E was never in the vector arm: absent is 0, not last-plus-one.
	last := got[3]
	if last.SpanID != "E" || last.VectorRank != 0 {
		t.Fatalf("E has vector rank %d, want 0", last.VectorRank)
	}
}

// k is the whole of what separates RRF from "sum of 1/rank", and it changes the
// answer exactly where one arm ranks a document very deep. X is (1st, 20th) and
// Y is (2nd, 2nd): with k=60 the deep rank is heavily discounted and Y wins;
// with k=0 the top rank dominates and X wins. A fixture where both arms agree
// cannot show this, because k then scales every score identically.
func TestKChangesTheOrderWhereItShould(t *testing.T) {
	vec := hits("X", "Y")
	lex := make([]Hit, 20)
	lex[0] = Hit{SpanID: "Z0", Path: "z0.go"}
	lex[1] = Hit{SpanID: "Y", Path: "Y.go"}
	for i := 2; i < 19; i++ {
		lex[i] = Hit{SpanID: "pad" + string(rune('a'+i)), Path: "pad.go"}
	}
	lex[19] = Hit{SpanID: "X", Path: "X.go"}

	if got := ids(Fuse(60, vec, lex))[0]; got != "Y" {
		t.Fatalf("k=60 ranked %q first, want Y", got)
	}
	if got := ids(Fuse(0, vec, lex))[0]; got != "X" {
		t.Fatalf("k=0 ranked %q first, want X", got)
	}
}

// Two documents at ranks (1,2) and (2,1) fuse to exactly the same score. The
// order must then come from the tie-break, not from map iteration or from the
// order the arms were walked in. P is walked first and sorts second, and P's id
// also sorts before Q's, so a tie-break on id alone fails here too.
func TestTiesBreakOnPathAndLineNotOnWalkOrder(t *testing.T) {
	p := Hit{SpanID: "aaa", Path: "a.go", StartLine: 10}
	q := Hit{SpanID: "bbb", Path: "a.go", StartLine: 5}
	for i := 0; i < 50; i++ {
		got := ids(Fuse(60, []Hit{p, q}, []Hit{q, p}))
		eq(t, got, []string{"bbb", "aaa"})
	}
}

// Vector-only mode is Fuse with one arm, and it must be order-preserving: RRF
// over a single list is a monotone function of rank, so anything that reorders
// it is a bug in the sum rather than a policy.
func TestSingleArmFusionPreservesThatArmsOrder(t *testing.T) {
	eq(t, ids(Fuse(60, hits("A", "B", "C"), nil)), []string{"A", "B", "C"})
	eq(t, ids(Fuse(60, nil, hits("C", "B", "A"))), []string{"C", "B", "A"})
}

// A span one arm never returned contributes nothing from that arm. Scoring it
// at len(list)+1 instead adds a near-constant to every span, which the fixture
// in TestFuseIsReciprocalRankOverBothArms cannot see: there the two synthetic
// terms are 1/65 and 1/64, and no pair is close enough to change places
// (D=1/64+1/64=0.031250 still trails E=1/65+1/63=0.031258).
//
// Here the arms have deliberately different lengths, so the synthetic terms do
// not cancel: "delta" is vector-only and would gain 1/65, "alpha" is
// lexical-only and would gain 1/63, and they sit 0.00026 apart, so the two
// swap. Expected order is neither insertion order (delta first) nor
// alphabetical (alpha first) nor path order (a.go first).
//
// k=60: nine=1/62+1/61=0.0325224, delta=1/61=0.0163934, alpha=1/62=0.0161290,
// zulu3=1/63=0.0158730, mike4=1/64=0.0156250.
func TestAbsentFromAnArmContributesNothing(t *testing.T) {
	vector := []Hit{{SpanID: "delta", Path: "m.go"}, {SpanID: "nine", Path: "z.go"}}
	lexical := []Hit{
		{SpanID: "nine", Path: "z.go"},
		{SpanID: "alpha", Path: "a.go"},
		{SpanID: "zulu3", Path: "b.go"},
		{SpanID: "mike4", Path: "c.go"},
	}
	eq(t, ids(Fuse(60, vector, lexical)), []string{"nine", "delta", "alpha", "zulu3", "mike4"})
}

// The claim the floor design rests on: a fused score says nothing about
// quality. Two runs with identical rank order and wildly different arm scores
// produce identical fused scores, so a floor on Score would be a floor on "did
// anything come back at all".
func TestFusedScoreCarriesNoQualitySignal(t *testing.T) {
	good := []Hit{{SpanID: "A", Path: "a.go", Score: 0.99}, {SpanID: "B", Path: "b.go", Score: 0.98}}
	junk := []Hit{{SpanID: "A", Path: "a.go", Score: 0.01}, {SpanID: "B", Path: "b.go", Score: 0.002}}
	g, j := Fuse(60, good, nil), Fuse(60, junk, nil)
	if g[0].Score != j[0].Score {
		t.Fatalf("a good arm fused to %v and a worthless one to %v", g[0].Score, j[0].Score)
	}
	// And the number the floor actually reads does differ.
	if g[0].VectorScore == j[0].VectorScore {
		t.Fatalf("both arms carried VectorScore %v", g[0].VectorScore)
	}
}
