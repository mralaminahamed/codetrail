package main

import (
	"context"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// newEmbedder builds this binary's embedder: embed.FromEnv, with the schema's
// width and store's own check for it.
//
// A wrapper rather than the call inlined at each site, because the fixtures
// build their embedder the way main does; if the two drifted, every job test
// would be running on an embedder production never constructs.
func newEmbedder(ctx context.Context, timeout time.Duration) (embed.Embedder, error) {
	return embed.FromEnv(ctx, store.EmbeddingDim, store.CheckDim, timeout)
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
//
// The first parse error wins over the range check below, because a value that
// never became a number has no range to be outside of.
func chunkOptions() (chunk.Options, error) {
	def := chunk.Defaults()
	var err error
	get := func(key string, d int) int {
		n, e := config.GetInt(key, d)
		if e != nil && err == nil {
			err = e
		}
		return n
	}
	opt := chunk.Options{
		Strategy:      chunk.Strategy(config.Get("CHUNK_STRATEGY", string(def.Strategy))),
		WindowLines:   get("CHUNK_WINDOW_LINES", def.WindowLines),
		WindowOverlap: get("CHUNK_WINDOW_OVERLAP", def.WindowOverlap),
		MaxDeclLines:  get("CHUNK_MAX_DECL_LINES", def.MaxDeclLines),
	}
	if err != nil {
		return chunk.Options{}, err
	}
	if err := opt.Validate(); err != nil {
		return chunk.Options{}, err
	}
	return opt, nil
}

// stripDocs reports whether doc comments are blanked before chunking.
//
// Production keeps them — they are the best retrieval signal a span has — and
// only the eval corpus strips them (spec §9). Through config.GetBool, which
// parses rather than comparing against "true": STRIP_DOC_COMMENTS=yes would
// otherwise read as false and build an eval corpus holding the very prose its
// questions came from, which scores both arms on string overlap and looks like
// a successful run.
func stripDocs() (bool, error) { return config.GetBool("STRIP_DOC_COMMENTS", false) }
