package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// newEmbedder builds the embedder from the environment and checks its width
// against the schema before any job runs. Discovered at the first insert
// instead, the same mismatch costs a clone, a walk and a whole embed pass
// first, and PutSpans's refusal names a span where this one names the setting.
//
// timeout is the client's own budget for one request. It is the job's deadline
// rather than a second, shorter number: the context already carries the job's
// deadline into every request, and a per-request timeout below it would end an
// embed the job still had time for.
func newEmbedder(timeout time.Duration) (embed.Embedder, error) {
	dim := config.GetInt("EMBED_DIM", store.EmbeddingDim)
	if err := store.CheckDim(dim); err != nil {
		return nil, fmt.Errorf("EMBED_DIM=%d: %w", dim, err)
	}
	model := config.Get("EMBED_MODEL", "nomic-embed-text")
	switch provider := config.Get("EMBED_PROVIDER", "ollama"); provider {
	case "ollama":
		// 11435 is what infra/docker-compose.yml publishes.
		return embed.NewOllama(config.Get("OLLAMA_URL", "http://localhost:11435"), model, dim, timeout), nil
	case "fake":
		// Spec §9: CI runs the harness with no model. The fake names itself in
		// embed_model, so a corpus built with it is identifiable in the data.
		return embed.NewFake(dim), nil
	default:
		return nil, fmt.Errorf("EMBED_PROVIDER must be ollama or fake, got %q", provider)
	}
}

// chunkOptions reads the chunk knobs and refuses a set the chunker would not
// accept, at boot rather than per job.
//
// config.GetInt reads a literal "0" as 0. Measured, not assumed: chunk.Chunks
// validates too, and answers "WindowLines must be positive, got 0" — so the
// counterfactual is not a corrupt corpus but every leased job failing on its
// first chunkable file and spending its attempts. That is the same trade
// limitsFrom makes: failing one boot is the diagnosable version of failing
// every repository in the queue.
func chunkOptions() (chunk.Options, error) {
	def := chunk.Defaults()
	opt := chunk.Options{
		Strategy:      chunk.Strategy(config.Get("CHUNK_STRATEGY", string(def.Strategy))),
		WindowLines:   config.GetInt("CHUNK_WINDOW_LINES", def.WindowLines),
		WindowOverlap: config.GetInt("CHUNK_WINDOW_OVERLAP", def.WindowOverlap),
		MaxDeclLines:  config.GetInt("CHUNK_MAX_DECL_LINES", def.MaxDeclLines),
	}
	if err := opt.Validate(); err != nil {
		return chunk.Options{}, err
	}
	return opt, nil
}

// stripDocs reports whether doc comments are blanked before chunking.
//
// Production keeps them — they are the best retrieval signal a span has — and
// only the eval corpus strips them (spec §9). Parsed rather than compared
// against "true": STRIP_DOC_COMMENTS=yes would otherwise read as false and
// build an eval corpus holding the very prose its questions came from, which
// scores both arms on string overlap and looks like a successful run.
func stripDocs() (bool, error) {
	v := config.Get("STRIP_DOC_COMMENTS", "false")
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("STRIP_DOC_COMMENTS must be a boolean, got %q", v)
	}
	return b, nil
}
