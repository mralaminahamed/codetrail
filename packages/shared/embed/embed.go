// Package embed turns span text into vectors.
//
// The interface has two implementations from the start because spec §9 requires
// CI to run the eval harness on a deterministic fake: the mechanics — batching,
// ordering, width, persistence, ranking — have to be provable with no model and
// no network, or they are only ever exercised on a developer's machine.
//
// Width is not negotiable. store.EmbeddingDim is a constant because an ANN index
// needs a fixed dimension, so a caller checks the embedder's Dim against it at
// startup (store.CheckDim) rather than discovering two vector spaces in one
// column later, where they rank nonsense confidently and no query looks wrong.
package embed

import "context"

// Embedder is the seam between the indexer and whatever produces vectors.
//
// Embed takes a batch and returns one vector per text, in order: the caller
// pairs vector i with span i, so an implementation that reorders or drops one
// files a span's text under another span's embedding.
//
// Model and Dim are recorded on every span row (embed_model, embed_dim), which
// makes a model switch a fact in the data rather than something inferred from a
// deploy date.
type Embedder interface {
	Model() string
	Dim() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
