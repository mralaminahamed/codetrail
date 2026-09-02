// Package rag holds the parts of retrieval that are pure functions: how two
// ranked lists become one, and whether the result is worth answering from.
//
// Nothing here touches a database, an embedder or a clock. That is what lets
// the eval (spec §9) run the same fusion and the same floor the gateway serves
// from, and it is also the only place the two arms can be told apart at all:
// under embed.Fake — a hashed bag of words over the same span text the lexical
// arm indexes — both arms compute lexical overlap, so no end-to-end test can
// show fusion doing anything.
package rag

import "fmt"

// Mode selects which arms run. Hybrid is the default because spec §8 defines
// retrieval as hybrid; that is a conformance choice, not a claim that fusion
// retrieves better, which spec:316 makes an experiment for P6/P7.
type Mode string

const (
	ModeVector  Mode = "vector"
	ModeLexical Mode = "lexical"
	ModeHybrid  Mode = "hybrid"
)

// ParseMode fails closed like chunk.Options.Validate. Defaulting an
// unrecognised mode would hide a typo behind a service that retrieves
// differently than it was asked to, which is unreadable from the outside.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeVector, ModeLexical, ModeHybrid:
		return Mode(s), nil
	}
	return "", fmt.Errorf("rag: unknown retrieval mode %q", s)
}

// Hit is one arm's result in that arm's own units: cosine similarity for the
// vector arm, ts_rank_cd for the lexical one. The two have no exchange rate,
// which is why Fuse reads their order and never these numbers.
type Hit struct {
	SpanID    string
	Path      string
	StartLine int
	Score     float64
}

// Fused is one span after both arms are combined.
//
// Score orders the result and carries no quality signal — see Fuse.
// VectorScore is the cosine similarity the floor is compared against and the
// number P6 calibrates from. The per-arm ranks travel to the caller so an
// arm's contribution is readable from the data rather than inferred
// (spec:316); rank 0 means that arm did not return the span at all.
type Fused struct {
	SpanID      string
	Path        string
	StartLine   int
	Score       float64
	VectorScore float64
	VectorRank  int
	LexicalRank int
}
