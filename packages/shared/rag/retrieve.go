package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// Searcher is the store surface retrieval needs, as an interface rather than
// *store.Store for two reasons that outlive the tests: spec §9 requires the
// eval to run *this* retriever rather than a reimplementation of it, and P2
// left open that the eval's two arms may end up in a database each. A
// retriever that takes a store takes either one.
type Searcher interface {
	VectorSearch(ctx context.Context, repoID string, q []float32, limit int) ([]models.Cite, error)
	LexicalSearch(ctx context.Context, repoID string, terms []string, limit int) ([]models.Cite, error)
	SpanEmbedder(ctx context.Context, repoID string) (model string, dim int, err error)
}

// ErrModelMismatch is a corpus that was indexed by an embedder other than the
// one embedding the query.
//
// An error, not a refusal. The two are then in different vector spaces, so
// every score is meaningless rather than low, and filing that under "we had
// nothing to say" is the confusion spec §10 forbids, read in the other
// direction.
var ErrModelMismatch = errors.New("rag: repo was indexed by another embedder")

// Retriever runs the configured arms over one repository and fuses them.
//
// Every field is configuration a deployment sets and P6 sweeps: Mode selects
// the arms (spec:316 makes fusion's value an experiment), K discounts deep
// ranks, Candidates is the per-arm depth, Split widens the lexical terms, and
// Floor travels with the result rather than being applied here — Decide is the
// handler's call, because only the handler knows whether the request was a
// search or an ask.
type Retriever struct {
	Store      Searcher
	Emb        embed.Embedder
	Mode       Mode
	K          int
	Candidates int
	Split      bool
	Floor      Floor
}

// Result is one retrieval, with everything a response and a decision need and
// nothing gateway-shaped.
//
// Spans holds the span behind each returned hit — models.Span and not
// models.Cite, because a Cite would carry whichever arm's score happened to be
// on the row, and a reader could not tell a cosine similarity from a
// ts_rank_cd. Every score that means something is on the Fused hit, where it
// says which arm it came from.
//
// TopScore is the vector arm's best cosine similarity and is NaN when that arm
// did not run or returned nothing. NaN rather than 0, because 0 is a real
// cosine similarity — an orthogonal span — and the difference is exactly what
// Decide refuses on.
type Result struct {
	Hits      []Fused
	Spans     map[string]models.Span
	TopScore  float64
	VectorRan bool
	Mode      Mode
}

// Search runs the arms, fuses them and trims to limit.
//
// Timing is recorded here and the outcome is not: this is the only thing that
// knows what "retrieval latency" covers, and the handler is the only thing
// that knows whether the request ended as an answer, a refusal or an error.
// Splitting them is what stops one call site counting both.
func (r *Retriever) Search(ctx context.Context, repoID, q string, limit int) (Result, error) {
	if err := r.validate(limit); err != nil {
		return Result{}, err
	}
	start := time.Now()
	res := Result{Mode: r.Mode, TopScore: math.NaN(), VectorRan: r.Mode != ModeLexical}

	var vector, lexical []models.Cite
	if res.VectorRan {
		qv, err := r.embedQuery(ctx, repoID, q)
		if err != nil {
			return Result{}, err
		}
		if vector, err = r.Store.VectorSearch(ctx, repoID, qv, r.Candidates); err != nil {
			return Result{}, err
		}
		if len(vector) > 0 {
			res.TopScore = float64(vector[0].Score)
		}
	}
	if r.Mode != ModeVector {
		var err error
		// The terms, never the question: to_tsquery reads &, |, !, : and ( as
		// operators, and most questions about code contain one.
		if lexical, err = r.Store.LexicalSearch(ctx, repoID, Terms(q, r.Split), r.Candidates); err != nil {
			return Result{}, err
		}
	}

	// Fuse over the full candidate depth and trim afterwards. Fusing two lists
	// of limit would make fusion an intersection test: a span the vector arm
	// ranks 30th and the lexical arm 1st is the case fusion exists for, and a
	// depth of limit never sees it.
	res.Hits = Fuse(r.K, hitsOf(vector), hitsOf(lexical))
	if len(res.Hits) > limit {
		res.Hits = res.Hits[:limit]
	}
	res.Spans = spansOf(res.Hits, vector, lexical)

	metrics.ObserveRetrieval(string(r.Mode), time.Since(start))
	metrics.ObserveTopScore(res.TopScore)
	return res, nil
}

// validate fails closed on the configuration Fuse cannot survive.
//
// Fuse takes k as an argument and has no guard of its own: k = -1 makes
// 1/(k+1) an infinity and k <= -2 inverts the ranking, silently. Both binaries
// validate RETRIEVAL_RRF_K at boot, and this is the second line for a
// Retriever built in code — which is what spec §9's eval does.
func (r *Retriever) validate(limit int) error {
	switch r.Mode {
	case ModeVector, ModeLexical, ModeHybrid:
	default:
		return fmt.Errorf("rag: unknown retrieval mode %q", r.Mode)
	}
	if r.K < 0 {
		return fmt.Errorf("rag: fusion k must not be negative, got %d", r.K)
	}
	if r.Candidates < 1 {
		return fmt.Errorf("rag: candidate depth must be positive, got %d", r.Candidates)
	}
	if limit < 1 {
		return fmt.Errorf("rag: limit must be positive, got %d", limit)
	}
	return nil
}

// embedQuery reads what the corpus was indexed with before embedding anything,
// so a mismatch costs no model call, and compares the width as well as the
// name: embed.Fake calls itself fake-hashed-bow at every width, so the name
// alone would admit a corpus in a different space under the same label.
func (r *Retriever) embedQuery(ctx context.Context, repoID, q string) ([]float32, error) {
	model, dim, err := r.Store.SpanEmbedder(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if model != r.Emb.Model() || dim != r.Emb.Dim() {
		return nil, fmt.Errorf("%w: repo %s holds %s/%d, the query is embedded with %s/%d",
			ErrModelMismatch, repoID, model, dim, r.Emb.Model(), r.Emb.Dim())
	}
	v, err := r.Emb.Embed(ctx, []string{q})
	if err != nil {
		return nil, err
	}
	if len(v) != 1 {
		return nil, fmt.Errorf("rag: embedder answered %d vectors for one query", len(v))
	}
	return v[0], nil
}

func hitsOf(cs []models.Cite) []Hit {
	out := make([]Hit, len(cs))
	for i, c := range cs {
		out[i] = Hit{SpanID: c.ID, Path: c.Path, StartLine: c.StartLine, Score: float64(c.Score)}
	}
	return out
}

// spansOf carries the text and digest of the spans that survived the trim, and
// only those: the arms returned Candidates rows each and a caller renders at
// most limit citations, so keeping the rest would hand the handler spans no
// hit references.
func spansOf(hits []Fused, arms ...[]models.Cite) map[string]models.Span {
	want := make(map[string]bool, len(hits))
	for _, h := range hits {
		want[h.SpanID] = true
	}
	out := make(map[string]models.Span, len(hits))
	for _, arm := range arms {
		for _, c := range arm {
			if want[c.ID] {
				out[c.ID] = c.Span
			}
		}
	}
	return out
}
