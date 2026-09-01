package rag

import "sort"

// Fuse combines the arms by reciprocal rank: score = Σ 1/(k + rank) over the
// arms that returned the span, ranks 1-based.
//
// Ranks, not scores, because the two arms have no common unit — a cosine
// similarity and a ts_rank_cd cannot be added, and normalising them would
// invent an exchange rate nobody measured. Spec §8 names this fusion;
// spec:316 makes whether it beats the vector arm alone an experiment for
// P6/P7, so every hit keeps the per-arm ranks that comparison needs.
//
// The returned Score is a function of ranks alone. It is deliberately not the
// number the floor reads: the top hit of any non-empty result scores 1/(k+1)
// whether it is a perfect match or the best of a worthless set.
//
// The arms are named parameters rather than a map[Arm][]Hit because spec §8
// closes the arm set, and because ranging a map would randomise the walk
// order — a determinism bug the tie-break below would mask until someone
// changed the sort.
func Fuse(k int, vector, lexical []Hit) []Fused {
	acc := make(map[string]*Fused, len(vector)+len(lexical))
	// Absent from an arm is no contribution from that arm, not a synthetic
	// rank of len+1: that would add a near-constant to every span and quietly
	// turn "fused" into "the other arm, damped".
	for i, h := range vector {
		f := entry(acc, h)
		f.VectorRank = i + 1
		f.VectorScore = h.Score
		f.Score += rrf(k, i+1)
	}
	for i, h := range lexical {
		f := entry(acc, h)
		f.LexicalRank = i + 1
		f.Score += rrf(k, i+1)
	}

	out := make([]Fused, 0, len(acc))
	for _, f := range acc {
		out = append(out, *f)
	}
	// Ties are ordinary, not exotic: (1st, 2nd) and (2nd, 1st) fuse to the
	// same score. Breaking on (path, line, id) keeps two runs over an
	// unchanged corpus diffable, and orders them the way a person reads a
	// repository. Span id is last because a hash orders nothing followable.
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
	return out
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
// is the parameter P6 sweeps, which is why it is an argument and not a const.
func rrf(k, rank int) float64 { return 1 / float64(k+rank) }
