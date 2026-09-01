package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// newEmbedder builds the embedder from the environment and proves it works
// before any job runs: the width against the schema, and — for a real server —
// one round trip that answers with a vector of that width.
//
// The round trip is what makes OLLAMA_URL and EMBED_MODEL boot-validated
// rather than job-validated. Measured before it: `OLLAMA_URL='not a url at
// all' EMBED_MODEL='no-such-model-xyz'` logged "indexer up" and the failure
// surfaced on the first leased job's embed call, spending an attempt against
// its cap for a setting no retry can fix. A worker that cannot embed cannot do
// its job, so refusing to start is the honest form of that.
//
// timeout is the client's own budget for one request. It is the job's deadline
// rather than a second, shorter number: the context already carries the job's
// deadline into every request, and a per-request timeout below it would end an
// embed the job still had time for.
func newEmbedder(ctx context.Context, timeout time.Duration) (embed.Embedder, error) {
	dim := config.GetInt("EMBED_DIM", store.EmbeddingDim)
	if err := store.CheckDim(dim); err != nil {
		return nil, fmt.Errorf("EMBED_DIM=%d: %w", dim, err)
	}
	model := config.Get("EMBED_MODEL", "nomic-embed-text")
	switch provider := config.Get("EMBED_PROVIDER", "ollama"); provider {
	case "ollama":
		// 11435 is what infra/docker-compose.yml publishes.
		raw := config.Get("OLLAMA_URL", "http://localhost:11435")
		// Parsed rather than handed to the client as-is: url.Parse accepts
		// "not a url at all" as a relative path, so the scheme and host are
		// what actually decide whether this is an address.
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("OLLAMA_URL=%q is not an http:// or https:// address", raw)
		}
		o := embed.NewOllama(raw, model, dim, timeout)
		if err := probe(ctx, o); err != nil {
			return nil, fmt.Errorf("EMBED_MODEL=%q at OLLAMA_URL=%s: %w", model, raw, err)
		}
		return o, nil
	case "fake":
		// Spec §9: CI runs the harness with no model. The fake names itself in
		// embed_model, so a corpus built with it is identifiable in the data.
		// Not probed: it has no server to be wrong about, and CheckDim above is
		// the whole of what could be misconfigured.
		return embed.NewFake(dim), nil
	default:
		return nil, fmt.Errorf("EMBED_PROVIDER must be ollama or fake, got %q", provider)
	}
}

// probeDeadline bounds that one round trip. Not the job deadline it is called
// with: that defaults to ten minutes, and a worker that hangs for ten minutes
// without saying why it did not start is worse than one that refuses.
//
// A var so a test can shorten it; nothing writes it in production.
var probeDeadline = 30 * time.Second

// probe embeds one short text and checks the answer's shape. A model that was
// never pulled answers 404 here, where the message can name EMBED_MODEL,
// rather than on the first job where it names a span.
func probe(ctx context.Context, e embed.Embedder) error {
	ctx, cancel := context.WithTimeout(ctx, probeDeadline)
	defer cancel()
	v, err := e.Embed(ctx, []string{"codetrail"})
	if err != nil {
		return err
	}
	if len(v) != 1 {
		return fmt.Errorf("answered %d vectors for 1 text", len(v))
	}
	if len(v[0]) != e.Dim() {
		return fmt.Errorf("answered a %d-wide vector, want %d", len(v[0]), e.Dim())
	}
	return nil
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
