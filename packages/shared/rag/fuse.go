package rag

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Params is everything fusion is configured by: the rank discount and one
// weight per arm.
//
// A struct rather than three scalars, because Fuse(k int, wv, wl float64, …)
// is two adjacent floats a call site can swap with no compiler complaint —
// invisible at the shipped 1.0 and wrong at every other point of a sweep.
//
// The weights ship at exactly 1.0 and are a GO FIELD, NOT A SETTING. There is
// no RETRIEVAL_W_VECTOR and no RETRIEVAL_W_LEXICAL: rag.Retriever's fields are
// all exported and apps/evalrunner constructs one directly (spec:43), so a
// sweep is `r2 := *r1; r2.Fusion.WLexical = 0.5` in one process. An env var
// would be a documented configuration surface with a boot validator and a
// README line, for a consumer that does not exist — which is the
// mode-request-field defect this project has already shipped once.
//
// Why a weight at all, now that the experiment has run and fusion LOST: the
// experiment measured one point on the 1:1 ratio, and "by how much" (spec:316)
// is still unanswerable at a fixed ratio. The result was one corpus of
// doc-comment prose queries, so a later re-run on a second golden set needs
// this field to say anything more than that one point.
type Params struct {
	K                 int
	WVector, WLexical float64
}

// DefaultParams is provably today's behaviour: 1.0 * x == x exactly in
// IEEE-754 for every finite x, so multiplying by a unit weight is not a
// rounding change and cannot re-rank a tied pair.
func DefaultParams() Params { return Params{K: 60, WVector: 1, WLexical: 1} }

// Validate fails closed on the configuration Fuse cannot survive.
//
// Zero is a MEANINGFUL sweep point — "lexical ranking with the vector arm still
// consulted for the floor", which differs from ModeLexical, where the floor
// does not apply at all. Negative is not: it inverts one arm's contribution
// silently, which is the hazard main.go's comment already records for k, and
// unlike k this has no env var and so no boot guard to inherit.
//
// Both weights zero fuses every span to score 0; the tie-break then orders by
// path and the caller gets a plausible-looking result set that is really no
// ranking at all.
func (p Params) Validate() error {
	if p.K < 0 {
		return fmt.Errorf("rag: fusion k must not be negative, got %d", p.K)
	}
	if math.IsNaN(p.WVector) || p.WVector < 0 {
		return fmt.Errorf("rag: fusion weight w_vector must not be negative, got %v", p.WVector)
	}
	if math.IsNaN(p.WLexical) || p.WLexical < 0 {
		return fmt.Errorf("rag: fusion weight w_lexical must not be negative, got %v", p.WLexical)
	}
	if p.WVector == 0 && p.WLexical == 0 {
		return errors.New("rag: fusion weights w_vector and w_lexical must not both be zero: that ranks nothing")
	}
	return nil
}

// Fuse combines the arms by reciprocal rank: score = Σ 1/(k + rank) over the
// arms that returned the span, ranks 1-based.
//
// Ranks, not scores, because the two arms have no common unit — a cosine
// similarity and a ts_rank_cd cannot be added, and normalising them would
// invent an exchange rate nobody measured. Spec §8 names this fusion;
// spec:316 made whether it beats the vector arm alone an experiment, and the
// experiment ran: on google/uuid @ 2d3c2a9, 74 cases, the AST arm's MRR was
// vector 0.7492 against hybrid 0.4023, so ModeVector is the default. Every hit
// still keeps the per-arm ranks, because that is what makes the comparison
// re-runnable on a corpus that is not one small library.
//
// The returned Score is a function of ranks alone. It is deliberately not the
// number the floor reads: the top hit of any non-empty result scores 1/(k+1)
// whether it is a perfect match or the best of a worthless set.
//
// The arms are named parameters rather than a map[Arm][]Hit because spec §8
// closes the arm set, and because ranging a map would randomise the walk
// order — a determinism bug the tie-break below would mask until someone
// changed the sort.
func Fuse(p Params, vector, lexical []Hit) []Fused {
	acc := make(map[string]*Fused, len(vector)+len(lexical))
	// Absent from an arm is no contribution from that arm, not a synthetic
	// rank of len+1: that would add a near-constant to every span and quietly
	// turn "fused" into "the other arm, damped".
	for i, h := range vector {
		f := entry(acc, h)
		f.VectorRank = i + 1
		f.VectorScore = h.Score
		f.Score += p.WVector * rrf(p.K, i+1)
	}
	for i, h := range lexical {
		f := entry(acc, h)
		f.LexicalRank = i + 1
		f.Score += p.WLexical * rrf(p.K, i+1)
	}

	out := make([]Fused, 0, len(acc))
	for _, f := range acc {
		out = append(out, *f)
	}
	sortFused(out)
	return out
}

// sortFused is the tie-break, extracted so a test can reproduce P3's whole
// computation rather than only its arithmetic.
//
// Ties are ordinary, not exotic: (1st, 2nd) and (2nd, 1st) fuse to the same
// score. Breaking on (path, line, id) keeps two runs over an unchanged corpus
// diffable, and orders them the way a person reads a repository. Span id is
// last because a hash orders nothing followable.
func sortFused(out []Fused) {
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		return a.SpanID < b.SpanID
	})
}

func entry(acc map[string]*Fused, h Hit) *Fused {
	f, ok := acc[h.SpanID]
	if !ok {
		f = &Fused{SpanID: h.SpanID, Path: h.Path, StartLine: h.StartLine}
		acc[h.SpanID] = f
	}
	return f
}

// k is the discount that stops one arm's top rank from dominating the sum. It
// is a field and not a const so an eval can sweep it; the one eval run so far
// held it at the paper's 60 and swept nothing, so no measurement here argues
// for any other value.
//
// The weight scales the arm's CONTRIBUTION and is applied outside this
// function, deliberately. Inside the denominator — 1/(w*k+rank) — it would
// change the shape of the rank-discount curve instead, so a sweep would
// measure something that is not a weight and report a curve for the wrong
// quantity.
func rrf(k, rank int) float64 { return 1 / float64(k+rank) }
