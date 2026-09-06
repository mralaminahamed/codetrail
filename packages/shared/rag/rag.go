// Package rag holds the parts of retrieval that are pure functions: how two
// ranked lists become one, and whether the result is worth answering from.
//
// Nothing here opens a connection or dials anything. The package imports no
// database driver and no HTTP client, and what it does need it is HANDED
// rather than constructs: Retriever holds a Searcher and an embed.Embedder,
// both interfaces, and every citation's clock arrives as NewCitation's `now`
// argument. The import graph is the check — neither pgx nor net/http appears
// in any file here. (Search does call Emb.Embed, and retrieve.go has one
// direct time.Now() for the latency histogram; no decision reads either.)
//
// That is what lets
// the eval (spec §9) run the same fusion and the same floor the gateway serves
// from, and it is also the only place the two arms can be told apart at all:
// under embed.Fake — a hashed bag of words over the same span text the lexical
// arm indexes — both arms compute lexical overlap, so no end-to-end test can
// show fusion doing anything.
package rag

import "fmt"

// Mode selects which arms run. All three ship and all three stay switchable.
//
// Vector is the default since P7, on evidence rather than on conformance: on
// google/uuid @ 2d3c2a9, 74 mechanically generated cases, live
// nomic-embed-text, the AST arm's MRR was vector 0.7492, hybrid 0.4023,
// lexical 0.1637. That is a deviation from spec:213-215, which defines
// retrieval as fused; spec:316 scopes the mechanism's VALUE as an experiment
// and the experiment ran. ONE CORPUS — see the README.
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
