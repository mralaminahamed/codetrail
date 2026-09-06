package embed

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/config"
)

// FromEnv builds the embedder from the environment and proves it works before
// the process serves anything: the width against the schema, and — for a real
// server — one round trip that answers with a vector of that width.
//
// Both binaries call it. The indexer embeds spans and the gateway embeds
// queries, and a query embedded by one model against a corpus embedded by
// another ranks nonsense confidently, so the two have to agree about all four
// knobs. They agreed by being written twice until P3; this is the one place.
//
// The round trip is what makes OLLAMA_URL and EMBED_MODEL boot-validated
// rather than request-validated. Measured before it: `OLLAMA_URL='not a url at
// all' EMBED_MODEL='no-such-model-xyz'` logged "indexer up" and the failure
// surfaced on the first leased job's embed call, spending an attempt against
// its cap for a setting no retry can fix.
//
// checkDim is the caller's, not this package's: the column the width has to
// match belongs to store, and taking the check as an argument keeps a database
// driver out of the embedder. It runs before anything is constructed, so a
// wrong EMBED_DIM names EMBED_DIM instead of spending a probe and blaming
// EMBED_MODEL for the answer's width.
//
// timeout is the client's own budget for one request: the indexer passes the
// job deadline the context already carries, the gateway a bound on the read
// path.
func FromEnv(ctx context.Context, schemaDim int, checkDim func(int) error, timeout time.Duration) (Embedder, error) {
	dim, err := config.GetInt("EMBED_DIM", schemaDim)
	if err != nil {
		return nil, err
	}
	if err := checkDim(dim); err != nil {
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
		o := NewOllama(raw, model, dim, timeout)
		if err := Probe(ctx, o); err != nil {
			return nil, fmt.Errorf("EMBED_MODEL=%q at OLLAMA_URL=%s: %w", model, raw, err)
		}
		return o, nil
	case "fake":
		// Spec §9: CI runs the harness with no model. The fake names itself in
		// embed_model, so a corpus built with it is identifiable in the data.
		// Not probed: it has no server to be wrong about, and the width check
		// above is the whole of what could be misconfigured.
		return NewFake(dim), nil
	default:
		return nil, fmt.Errorf("EMBED_PROVIDER must be ollama or fake, got %q", provider)
	}
}

// probeDeadline bounds that one round trip. Not the caller's timeout: the
// indexer's is the job deadline, which defaults to ten minutes, and a process
// that hangs for ten minutes without saying why it did not start is worse than
// one that refuses.
//
// A var so a test can shorten it; nothing writes it in production.
var probeDeadline = 30 * time.Second

// Probe embeds one short text and checks the answer's shape. A model that was
// never pulled answers 404 here, where the message can name EMBED_MODEL,
// rather than on the first job where it names a span.
//
// Exported because the gateway's /ready runs it too: boot proves the embedder
// once, and a readiness check that proved it a second way would be a second
// definition of "working", agreeing with this one only on the day it was
// written.
func Probe(ctx context.Context, e Embedder) error {
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
