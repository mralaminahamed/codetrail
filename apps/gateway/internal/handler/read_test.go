package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

const (
	fixtureRepoID = "repo-1"
	fixtureCommit = "dfd11cca1234567890abcdef1234567890abcdef"
	fixtureDim    = 16
)

var (
	fixtureIndexedAt = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fixtureNow       = fixtureIndexedAt.Add(72 * time.Hour)

	fixtureRepo = models.Repo{
		ID: fixtureRepoID, Remote: "https://github.com/rs/zerolog", Ref: "main",
		Commit: fixtureCommit, SizeBytes: 4096, IndexedAt: fixtureIndexedAt,
	}
)

// Three spans (never one: a single-document corpus cannot detect a ranking
// bug), whose intended rank order disagrees with path order and with span-id
// order. Each digest is the real hash of its text, because the assertions below
// are about whether the text served is the text the digest covers.
var fixtureSpans = []models.Span{
	newSpan("span-c", "internal/sampler.go", "Sample", 42, 61,
		"func (s *Sampler) Sample(lvl Level) bool { return s.burst > 0 }"),
	newSpan("span-a", "core/log.go", "Logger", 10, 18,
		"type Logger struct { w LevelWriter; sampler Sampler }"),
	newSpan("span-b", "docs/sampling.md", "", 1, 4,
		"Sampling drops events once the burst budget is spent."),
}

func newSpan(id, path, symbol string, start, end int, text string) models.Span {
	sum := sha256.Sum256([]byte(text))
	return models.Span{
		ID: id, RepoID: fixtureRepoID, FileID: "file-" + id, Path: path,
		Kind: models.KindFunc, Symbol: symbol, StartLine: start, EndLine: end,
		Text: text, Digest: hex.EncodeToString(sum[:]),
	}
}

func spanByID(id string) models.Span {
	for _, s := range fixtureSpans {
		if s.ID == id {
			return s
		}
	}
	panic("no fixture span " + id)
}

// fakeStore is both the read path's store and, so the tokenless-question test
// can run a real Retriever over a real embed.Fake, the two retrieval arms.
type fakeStore struct {
	repos map[string]models.Repo
	rows  []store.RepoRow
	stats store.Stats
	spans map[string]models.Span
	gone  map[string]bool
	newer rag.Newer
	err   error

	touched  []string
	touchErr error

	vector, lexical []models.Cite
	model           string
	dim             int
	// embedderErr is what a repository with no spans answers: the real store
	// reports that as ErrNotFound, and it is the one arm failure the handler
	// must not read as a failure at all.
	embedderErr error
}

func newStore() *fakeStore {
	spans := make(map[string]models.Span, len(fixtureSpans))
	for _, s := range fixtureSpans {
		spans[s.ID] = s
	}
	return &fakeStore{
		repos: map[string]models.Repo{fixtureRepoID: fixtureRepo},
		rows:  []store.RepoRow{{Repo: fixtureRepo, LastUsedAt: fixtureNow}},
		stats: store.Stats{Files: 12, Spans: 30, FilesWithSpans: 9},
		spans: spans,
		gone:  map[string]bool{},
		model: "fake-hashed-bow", dim: fixtureDim,
	}
}

func (f *fakeStore) ListRepos(context.Context, int) ([]store.RepoRow, error) {
	return f.rows, f.err
}

func (f *fakeStore) GetRepo(_ context.Context, id string) (models.Repo, error) {
	if f.err != nil {
		return models.Repo{}, f.err
	}
	r, ok := f.repos[id]
	if !ok {
		return models.Repo{}, store.ErrNotFound
	}
	return r, nil
}

// RepoGone answers the tombstone table only. The live-repo guard the real one
// carries (a NOT EXISTS against repos) is deliberately absent here, so a
// handler that asks in the wrong order is visible rather than covered for.
func (f *fakeStore) RepoGone(_ context.Context, id string) (bool, error) {
	return f.gone[id], f.err
}

func (f *fakeStore) RepoStats(context.Context, string) (store.Stats, error) {
	return f.stats, f.err
}

func (f *fakeStore) GetSpan(_ context.Context, _, spanID string) (models.Span, error) {
	if f.err != nil {
		return models.Span{}, f.err
	}
	s, ok := f.spans[spanID]
	if !ok {
		return models.Span{}, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeStore) NewerCommit(context.Context, string) (string, time.Time, error) {
	return f.newer.Commit, f.newer.IndexedAt, f.err
}

func (f *fakeStore) TouchRepo(_ context.Context, id string) error {
	f.touched = append(f.touched, id)
	return f.touchErr
}

func (f *fakeStore) VectorSearch(context.Context, string, []float32, int) ([]models.Cite, error) {
	return f.vector, f.err
}

func (f *fakeStore) LexicalSearch(context.Context, string, []string, int) ([]models.Cite, error) {
	return f.lexical, f.err
}

func (f *fakeStore) SpanEmbedder(context.Context, string) (string, int, error) {
	if f.embedderErr != nil {
		return "", 0, f.embedderErr
	}
	return f.model, f.dim, f.err
}

type retrieverCall struct {
	repoID, q string
	limit     int
}

type fakeRetriever struct {
	res   rag.Result
	err   error
	calls []retrieverCall
}

func (f *fakeRetriever) Search(_ context.Context, repoID, q string, limit int) (rag.Result, error) {
	f.calls = append(f.calls, retrieverCall{repoID, q, limit})
	if f.err != nil {
		return rag.Result{}, f.err
	}
	return f.res, nil
}

// result builds one retrieval whose per-arm ranks differ, so a response that
// drops or transposes them is visible. span-b is lexical-only: rank 0 in the
// vector arm is how Fuse spells "that arm did not have it".
func result(mode rag.Mode, top float64, vectorRan bool) rag.Result {
	spans := map[string]models.Span{}
	for _, s := range fixtureSpans {
		spans[s.ID] = s
	}
	return rag.Result{
		Hits: []rag.Fused{
			{SpanID: "span-c", Path: "internal/sampler.go", StartLine: 42,
				Score: 0.032, VectorScore: top, VectorRank: 1, LexicalRank: 3},
			{SpanID: "span-a", Path: "core/log.go", StartLine: 10,
				Score: 0.031, VectorScore: 0.61, VectorRank: 2, LexicalRank: 1},
			{SpanID: "span-b", Path: "docs/sampling.md", StartLine: 1,
				Score: 0.016, VectorRank: 0, LexicalRank: 2},
		},
		Spans: spans, TopScore: top, VectorRan: vectorRan, Mode: mode,
	}
}

func hermeticHandler(st *fakeStore, rt Retriever) *Handler {
	return &Handler{
		Repos: st, Rag: rt, Floor: rag.DefaultFloor(), Budget: rag.DefaultBudget(),
		Now: func() time.Time { return fixtureNow },
	}
}

// mount carries the middleware server.New does, because the 500 path's promise
// is a request id a caller can quote and without RequestID it is empty — and
// because a handler that panics answers 500 in production, not by unwinding
// into the test binary.
func mount(h *Handler) *echo.Echo {
	e := echo.New()
	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	Mount(e, h)
	return e
}

// reads a JSON body or fails the test, so no assertion below runs against a
// zero value it mistook for a response.
func body(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestSearchReturnsPerArmRanksSoFusionCanBeMeasured(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	rec := do(mount(hermeticHandler(st, rt)), http.MethodPost,
		"/api/repos/repo-1/search", `{"q":"how does the sampler drop an event","limit":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		RepoID   string   `json:"repo_id"`
		Mode     string   `json:"mode"`
		TopScore *float64 `json:"top_score"`
		Count    int      `json:"count"`
		Hits     []struct {
			SpanID      string       `json:"span_id"`
			Path        string       `json:"path"`
			StartLine   int          `json:"start_line"`
			EndLine     int          `json:"end_line"`
			Text        string       `json:"text"`
			VectorScore *float64     `json:"vector_score"`
			VectorRank  int          `json:"vector_rank"`
			LexicalRank int          `json:"lexical_rank"`
			Citation    rag.Citation `json:"citation"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.RepoID != fixtureRepoID || out.Mode != "hybrid" || out.Count != 3 {
		t.Errorf("repo %q mode %q count %d", out.RepoID, out.Mode, out.Count)
	}
	if out.TopScore == nil || *out.TopScore != 0.83 {
		t.Errorf("top_score %v, want 0.83", out.TopScore)
	}
	// Rank, not merely presence: the whole point of the per-arm ranks is that
	// an arm's contribution is readable, and a response that served the same
	// two numbers for every hit would satisfy a presence check.
	for i, want := range []struct {
		id                string
		vecRank, lexRank  int
		vectorScoreIsNull bool
	}{
		{"span-c", 1, 3, false},
		{"span-a", 2, 1, false},
		{"span-b", 0, 2, true},
	} {
		got := out.Hits[i]
		if got.SpanID != want.id {
			t.Errorf("rank %d is %q, want %q", i+1, got.SpanID, want.id)
			continue
		}
		if got.VectorRank != want.vecRank || got.LexicalRank != want.lexRank {
			t.Errorf("%s: ranks (v%d, l%d), want (v%d, l%d)",
				got.SpanID, got.VectorRank, got.LexicalRank, want.vecRank, want.lexRank)
		}
		// A span the vector arm never returned has no cosine similarity, and 0
		// there is a real one — an orthogonal span.
		if (got.VectorScore == nil) != want.vectorScoreIsNull {
			shown := "null"
			if got.VectorScore != nil {
				shown = strconv.FormatFloat(*got.VectorScore, 'g', -1, 64)
			}
			t.Errorf("%s: vector_score %s with vector_rank %d", got.SpanID, shown, got.VectorRank)
		}
		s := spanByID(want.id)
		if got.Text != s.Text || got.StartLine != s.StartLine || got.EndLine != s.EndLine {
			t.Errorf("%s: served %q at %d-%d", got.SpanID, got.Text, got.StartLine, got.EndLine)
		}
		if got.Citation.Digest != s.Digest {
			t.Errorf("%s: digest %s, want %s", got.SpanID, got.Citation.Digest, s.Digest)
		}
	}
	// The fixture's ranking disagrees with path order, so a handler that
	// re-sorted by path would not pass by coincidence.
	if out.Hits[0].Path < out.Hits[1].Path {
		t.Fatal("fixture: rank order agrees with path order")
	}
	// The limit reaches the retriever; a handler that ignored it would retrieve
	// its own default and nothing in the body would say so.
	if len(rt.calls) != 1 || rt.calls[0].limit != 3 || rt.calls[0].repoID != fixtureRepoID {
		t.Errorf("retriever calls %+v", rt.calls)
	}
}

// The floor is the answer's decision. Search ranks: it returns what it found
// with the scores attached, and never a refusal.
func TestSearchDoesNotRefuse(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.2, true)}
	h := hermeticHandler(st, rt)
	h.Floor = rag.Floor{Value: 0.9, Calibrated: false}
	rec := do(mount(h), http.MethodPost, "/api/repos/repo-1/search", `{"q":"sampler"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	out := body(t, rec)
	if _, ok := out["refused"]; ok {
		t.Errorf("search answered a refusal: %s", rec.Body)
	}
	if _, ok := out["floor"]; ok {
		t.Errorf("search reports a floor it never applied: %s", rec.Body)
	}
	if hits, ok := out["hits"].([]any); !ok || len(hits) != 3 {
		t.Errorf("hits %v", out["hits"])
	}
}

func TestAskAnswersWithCitationsAndAPermalink(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	rec := do(mount(hermeticHandler(st, rt)), http.MethodPost,
		"/api/repos/repo-1/ask", `{"q":"how does the sampler drop an event"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		RepoID     string      `json:"repo_id"`
		Refused    bool        `json:"refused"`
		AnsweredBy string      `json:"answered_by"`
		Answer     string      `json:"answer"`
		Citations  []rag.Cited `json:"citations"`
		Dropped    int         `json:"dropped"`
		TopScore   *float64    `json:"top_score"`
		Floor      struct {
			Value      float64 `json:"value"`
			Calibrated bool    `json:"calibrated"`
			Applicable bool    `json:"applicable"`
		} `json:"floor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Refused {
		t.Fatalf("refused: %s", rec.Body)
	}
	if out.AnsweredBy != "extractive" {
		t.Errorf("answered_by %q: the field has to exist before there is anything to downgrade from", out.AnsweredBy)
	}
	// The floor travels with the answer and says it is not calibrated. A
	// response that looked confidently thresholded at -1 is the overclaim this
	// project treats as a defect.
	if out.Floor.Value != -1 || out.Floor.Calibrated || !out.Floor.Applicable {
		t.Errorf("floor %+v, want {-1 false true}", out.Floor)
	}
	if len(out.Citations) != 3 || out.Dropped != 0 {
		t.Fatalf("%d citations, %d dropped: %s", len(out.Citations), out.Dropped, rec.Body)
	}
	for i, c := range out.Citations {
		s := spanByID(c.SpanID)
		if c.Marker != i+1 {
			t.Errorf("citation %d carries marker %d", i+1, c.Marker)
		}
		// Values, not presence: a citation with the right shape and the wrong
		// lines points at code that does not say what the answer says.
		if c.Citation.StartLine != s.StartLine || c.Citation.EndLine != s.EndLine {
			t.Errorf("%s cites %d-%d, want %d-%d", c.SpanID,
				c.Citation.StartLine, c.Citation.EndLine, s.StartLine, s.EndLine)
		}
		if c.Citation.Digest != s.Digest {
			t.Errorf("%s: digest %s, want %s", c.SpanID, c.Citation.Digest, s.Digest)
		}
		if c.Citation.Commit != fixtureCommit {
			t.Errorf("%s: commit %s", c.SpanID, c.Citation.Commit)
		}
		want := "https://github.com/rs/zerolog/blob/" + fixtureCommit + "/" +
			s.Path + "#L" + strconv.Itoa(s.StartLine) + "-L" + strconv.Itoa(s.EndLine)
		if c.Citation.Permalink != want {
			t.Errorf("%s: permalink %q, want %q", c.SpanID, c.Citation.Permalink, want)
		}
		if !strings.Contains(out.Answer, s.Text) {
			t.Errorf("%s: the cited text is not in the answer", c.SpanID)
		}
	}
	// Staleness asserts only what the corpus knows.
	st0 := out.Citations[0].Citation.Staleness
	if st0.State != rag.StaleUnknown || st0.ForgeChecked {
		t.Errorf("staleness %+v: unknown never means fresh", st0)
	}
	if !strings.Contains(st0.Note, "has not checked") {
		t.Errorf("staleness note %q", st0.Note)
	}
	if out.TopScore == nil || *out.TopScore != 0.83 {
		t.Errorf("top_score %v", out.TopScore)
	}
}

// A refusal is a 200 with a reason from a closed set. Expressing it as a 4xx
// would file "we had nothing to say" inside every error-rate panel, which is
// the confusion spec §10 exists to prevent.
func TestAskRefusesUnderTheFloorWithTwoHundredAndAReason(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.2, true)}
	h := hermeticHandler(st, rt)
	h.Floor = rag.Floor{Value: 0.5, Calibrated: false}
	rec := do(mount(h), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 with refused=true, got %d: %s", rec.Code, rec.Body)
	}
	out := body(t, rec)
	if out["refused"] != true {
		t.Fatalf("refused %v: %s", out["refused"], rec.Body)
	}
	// Which reason, not merely that a refusal happened: the three reasons are
	// three different facts about the corpus.
	if out["reason"] != string(rag.ReasonBelowFloor) {
		t.Errorf("reason %v, want %q", out["reason"], rag.ReasonBelowFloor)
	}
	if d, _ := out["detail"].(string); !strings.Contains(d, "not calibrated") {
		t.Errorf("detail %q says nothing about the floor being uncalibrated", d)
	}
	floor, _ := out["floor"].(map[string]any)
	if floor["value"] != 0.5 || floor["calibrated"] != false || floor["applicable"] != true {
		t.Errorf("floor %v", floor)
	}
	if _, ok := out["answer"]; ok {
		t.Errorf("a refusal carries an answer: %s", rec.Body)
	}
}

func TestAskRefusesWhenNothingWasRetrieved(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: rag.Result{Mode: rag.ModeHybrid, TopScore: math.NaN(), VectorRan: true}}
	rec := do(mount(hermeticHandler(st, rt)), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	out := body(t, rec)
	if out["refused"] != true || out["reason"] != string(rag.ReasonNoSpans) {
		t.Fatalf("refused %v reason %v, want true and %q", out["refused"], out["reason"], rag.ReasonNoSpans)
	}
	// An empty result has no cosine similarity. json.Marshal fails outright on
	// NaN, so the honest encoding is null.
	if v, ok := out["top_score"]; !ok || v != nil {
		t.Errorf("top_score %v, want null", v)
	}
}

// A repository indexed with no spans is the likeliest way a caller meets spec
// §10's distinction, and it used to be a 500: SpanEmbedder answers ErrNotFound
// for a corpus with nothing in it, and the retrieval path filed that as a
// failure — which moved the error counter and made no_spans unreachable in
// every mode that runs the vector arm.
//
// A real Retriever over the fake store, not a stubbed one: the outcome turns on
// what the store answers, and a fake retriever returning a refusal directly
// would assert nothing about the path that produces it. Both affected modes,
// because vector-only reaches the same call and a fix keyed on hybrid would
// leave it broken.
//
// The reason is asserted, not merely the refusal: the three reasons are three
// different facts about the corpus, and a mutant refusing for another one
// passes a check that only reads refused:true.
func TestARepoWithNoSpansRefusesRatherThanErrors(t *testing.T) {
	for _, mode := range []rag.Mode{rag.ModeHybrid, rag.ModeVector} {
		t.Run(string(mode), func(t *testing.T) {
			st := newStore()
			// Wrapped the way the real store wraps it, so a handler comparing
			// with == rather than errors.Is fails here.
			st.embedderErr = fmt.Errorf("%w: repo %s has no spans", store.ErrNotFound, fixtureRepoID)
			h := hermeticHandler(st, &rag.Retriever{
				Store: st, Emb: embed.NewFake(fixtureDim), Mode: mode,
				K: 60, Candidates: 40, Split: true, Floor: rag.DefaultFloor(),
			})

			before := counters(t)
			rec := do(mount(h), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
			moved := movedSince(t, before)
			if rec.Code != http.StatusOK {
				t.Fatalf("an empty corpus is a refusal, got %d: %s", rec.Code, rec.Body)
			}
			out := body(t, rec)
			if out["refused"] != true || out["reason"] != string(rag.ReasonNoSpans) {
				t.Errorf("refused %v reason %v, want true and %q", out["refused"], out["reason"], rag.ReasonNoSpans)
			}
			if d, _ := out["detail"].(string); d == "" {
				t.Errorf("a refusal with no detail: %s", rec.Body)
			}
			// The response still says what ran and that it scored nothing: a
			// refusal built from a zero Result would report mode "" and a
			// top score of 0, which is a real cosine similarity.
			if out["mode"] != string(mode) {
				t.Errorf("mode %v, want %q", out["mode"], mode)
			}
			if v, ok := out["top_score"]; !ok || v != nil {
				t.Errorf("top_score %v, want null", v)
			}
			// The whole movement, so a mutant that also counts an error fails.
			want := map[string]float64{
				`codetrail_answer_total{outcome="refused"}`:  1,
				`codetrail_refusal_total{reason="no_spans"}`: 1,
			}
			if !maps.Equal(moved, want) {
				t.Errorf("counters moved %v, want %v", moved, want)
			}

			// The same corpus on the search route ranks nothing, which is an
			// empty list and not a failure either.
			rec = do(mount(h), http.MethodPost, "/api/repos/repo-1/search", `{"q":"sampler"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("search over an empty corpus: want 200, got %d: %s", rec.Code, rec.Body)
			}
			out = body(t, rec)
			if out["count"] != float64(0) || out["mode"] != string(mode) {
				t.Errorf("search body %s", rec.Body)
			}
		})
	}
}

// The two that carry spec §10, read in both directions. Each asserts the delta
// on *both* counters: a test that only checks its own passes under a mutant
// that increments both.
func TestARetrievalErrorIsFiveHundredAndIsNotCountedAsARefusal(t *testing.T) {
	st := newStore()
	rt := &fakeRetriever{err: errors.New("connection refused: postgres://codetrail@db:5432")}
	before := counters(t)
	rec := do(mount(hermeticHandler(st, rt)), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
	moved := movedSince(t, before)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rec.Code, rec.Body)
	}
	out := body(t, rec)
	// P1's disclosure rule: the caller gets an opaque body and a request id.
	if out["error"] != "internal error" || out["request_id"] == "" {
		t.Errorf("500 body %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "postgres://") {
		t.Errorf("the 500 body carries the underlying error: %s", rec.Body)
	}
	want := map[string]float64{`codetrail_answer_total{outcome="error"}`: 1}
	if !maps.Equal(moved, want) {
		t.Errorf("counters moved %v, want %v", moved, want)
	}
}

func TestARefusalIsNotCountedAsAnError(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.2, true)}
	h := hermeticHandler(st, rt)
	h.Floor = rag.Floor{Value: 0.5}
	before := counters(t)
	rec := do(mount(h), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
	moved := movedSince(t, before)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	want := map[string]float64{
		`codetrail_answer_total{outcome="refused"}`:     1,
		`codetrail_refusal_total{reason="below_floor"}`: 1,
	}
	if !maps.Equal(moved, want) {
		t.Errorf("counters moved %v, want %v", moved, want)
	}
}

func TestAnAnsweredQuestionIsCountedAsAnswered(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	before := counters(t)
	rec := do(mount(hermeticHandler(st, rt)), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
	moved := movedSince(t, before)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	want := map[string]float64{`codetrail_answer_total{outcome="answered"}`: 1}
	if !maps.Equal(moved, want) {
		t.Errorf("counters moved %v, want %v", moved, want)
	}
}

// A rejected question never reached the answerer. Counting it as an error would
// inflate the rate spec §10 wants readable with callers' typing mistakes.
func TestAValidationFailureIsCountedAsNoAnswerOutcomeAtAll(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	before := counters(t)
	do(mount(hermeticHandler(st, rt)), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"???"}`)
	if moved := movedSince(t, before); len(moved) != 0 {
		t.Errorf("a 400 moved %v", moved)
	}
}

// The tokenless case runs against a real embed.Fake, which refuses a text it
// can hash no token from. A stubbed retriever would swallow the mutation: it
// returns hits whatever it is asked, so removing the validation would still
// answer 200 and the test would pass.
func TestAnEmptyOrTokenlessQuestionIsFourHundredNamingTheRule(t *testing.T) {
	for _, tc := range []struct{ name, q string }{
		{"empty", ""},
		{"whitespace", "   \t\n "},
		{"punctuation only", "???"},
		{"too long", strings.Repeat("a", maxQuestionLen+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.vector = []models.Cite{{Span: spanByID("span-c"), Score: 0.9}}
			real := &rag.Retriever{
				Store: st, Emb: embed.NewFake(fixtureDim), Mode: rag.ModeHybrid,
				K: 60, Candidates: 40, Floor: rag.DefaultFloor(),
			}
			q, err := json.Marshal(map[string]string{"q": tc.q})
			if err != nil {
				t.Fatal(err)
			}
			for _, route := range []string{"search", "ask"} {
				rec := do(mount(hermeticHandler(st, real)), http.MethodPost,
					"/api/repos/repo-1/"+route, string(q))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("%s: want 400 with a rule, got %d: %s", route, rec.Code, rec.Body)
				}
				out := body(t, rec)
				if out["rule"] != "form" || out["error"] == "" {
					t.Errorf("%s: body %s does not name the rule", route, rec.Body)
				}
			}
		})
	}
}

// Outside 1..50 is a 400, not a silent clamp: a caller asking for 5,000 spans
// has misunderstood something, and quietly serving 50 hides it.
func TestALimitOutsideTheBoundsIsFourHundredAndNotClamped(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	for _, tc := range []struct{ name, method, path, body string }{
		{"search zero", http.MethodPost, "/api/repos/repo-1/search", `{"q":"sampler","limit":0}`},
		{"search over", http.MethodPost, "/api/repos/repo-1/search", `{"q":"sampler","limit":5000}`},
		{"search negative", http.MethodPost, "/api/repos/repo-1/search", `{"q":"sampler","limit":-1}`},
		{"list over", http.MethodGet, "/api/repos?limit=51", ""},
		{"list zero", http.MethodGet, "/api/repos?limit=0", ""},
		{"list not a number", http.MethodGet, "/api/repos?limit=all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(mount(hermeticHandler(st, rt)), tc.method, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
			}
			if out := body(t, rec); out["rule"] != "form" {
				t.Errorf("body %s does not name the rule", rec.Body)
			}
		})
	}
}

func TestAnUnknownRepoIsFourOhFour(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	for _, r := range repoRoutes() {
		rec := do(mount(hermeticHandler(st, rt)), r.method, r.path("nobody"), r.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: want 404, got %d: %s", r.method, r.path("nobody"), rec.Code, rec.Body)
		}
	}
}

// 410, not 404: it existed, and that is a different fact (spec §10).
func TestAnEvictedRepoIsFourTenNotFourOhFour(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	st.gone["evicted-1"] = true
	for _, r := range repoRoutes() {
		rec := do(mount(hermeticHandler(st, rt)), r.method, r.path("evicted-1"), r.body)
		if rec.Code != http.StatusGone {
			t.Errorf("%s %s: want 410, got %d: %s", r.method, r.path("evicted-1"), rec.Code, rec.Body)
		}
		if out := body(t, rec); !strings.Contains(out["error"].(string), "evicted") {
			t.Errorf("410 body %s does not say what happened", rec.Body)
		}
	}
}

// A repo id is hash(key, commit), so re-indexing the same commit re-uses the id
// its tombstone was written under. A handler that asks "is it gone" before it
// reads repos answers 410 for a repository that is right there — and this fake
// carries no NOT EXISTS guard, so the order is the only thing that can save it.
func TestALiveRepoIsNeverGoneEvenWithATombstone(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	st.gone[fixtureRepoID] = true
	for _, r := range repoRoutes() {
		rec := do(mount(hermeticHandler(st, rt)), r.method, r.path(fixtureRepoID), r.body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: want 200 for a live repo with a stale tombstone, got %d: %s",
				r.method, r.path(fixtureRepoID), rec.Code, rec.Body)
		}
	}
}

type repoRoute struct {
	method string
	path   func(repo string) string
	body   string
}

func repoRoutes() []repoRoute {
	return []repoRoute{
		{http.MethodGet, func(r string) string { return "/api/repos/" + r }, ""},
		{http.MethodGet, func(r string) string { return "/api/repos/" + r + "/spans/span-c" }, ""},
		{http.MethodPost, func(r string) string { return "/api/repos/" + r + "/search" }, `{"q":"sampler"}`},
		{http.MethodPost, func(r string) string { return "/api/repos/" + r + "/ask" }, `{"q":"sampler"}`},
	}
}

// TouchRepo is the LRU clock spec §4 designed and P1 shipped with nothing to
// wind. Nothing about a response changes when it stops running, so no assertion
// on a body can see this.
func TestASuccessfulReadWindsTheLRUClock(t *testing.T) {
	for _, r := range repoRoutes() {
		st, rt := newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
		rec := do(mount(hermeticHandler(st, rt)), r.method, r.path(fixtureRepoID), r.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d: %s", r.path(fixtureRepoID), rec.Code, rec.Body)
		}
		if len(st.touched) != 1 || st.touched[0] != fixtureRepoID {
			t.Errorf("%s: TouchRepo was not called (touched %v)", r.path(fixtureRepoID), st.touched)
		}
	}

	// A refusal is a use of the repository too.
	st, rt := newStore(), &fakeRetriever{res: rag.Result{Mode: rag.ModeHybrid, TopScore: math.NaN(), VectorRan: true}}
	do(mount(hermeticHandler(st, rt)), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
	if len(st.touched) != 1 {
		t.Errorf("a refusal did not wind the clock: touched %v", st.touched)
	}

	// And a failed touch is a hint that was lost, not an answer that was: the
	// caller still gets the answer. A test that only asserted the call happened
	// would pass with the error returned to the caller.
	st, rt = newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}
	st.touchErr = errors.New("deadlock detected")
	var logged bytes.Buffer
	h := hermeticHandler(st, rt)
	h.Log = zerolog.New(&logged).Level(zerolog.InfoLevel)
	rec := do(mount(h), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a failed touch cost the caller the answer: %d %s", rec.Code, rec.Body)
	}
	if out := body(t, rec); out["refused"] != false || out["answer"] == "" {
		t.Errorf("no answer behind the 200: %s", rec.Body)
	}
	if !strings.Contains(logged.String(), "deadlock detected") {
		t.Errorf("a lost touch was dropped silently: %s", logged.String())
	}
}

// A question is user input on a public endpoint and the one string in this
// phase that must not reach an operator's log aggregator. A request id is what
// ties a report to a line.
func TestTheQuestionIsNeverLogged(t *testing.T) {
	const secret = "zqxjkv-sampler-question"
	for _, tc := range []struct {
		name, path, body string
		rt               *fakeRetriever
	}{
		{"answered", "/api/repos/repo-1/ask", `{"q":"` + secret + `"}`,
			&fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}},
		{"refused", "/api/repos/repo-1/ask", `{"q":"` + secret + `"}`,
			&fakeRetriever{res: rag.Result{Mode: rag.ModeHybrid, TopScore: math.NaN(), VectorRan: true}}},
		{"searched", "/api/repos/repo-1/search", `{"q":"` + secret + `"}`,
			&fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}},
		// The 500 path logs the real error, which is the one line that has to
		// be written; it must still not carry the question.
		{"failed", "/api/repos/repo-1/ask", `{"q":"` + secret + `"}`,
			&fakeRetriever{err: errors.New("connection refused")}},
		{"rejected", "/api/repos/repo-1/ask", `{"q":"` + secret + `` + strings.Repeat("x", maxQuestionLen) + `"}`,
			&fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}},
		{"malformed", "/api/repos/repo-1/ask", `{"q":"` + secret + `"`,
			&fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			h := hermeticHandler(newStore(), tc.rt)
			// logger.New pins production to InfoLevel; a test logger that
			// accepted more would fail on a line production never writes.
			h.Log = zerolog.New(&logged).Level(zerolog.InfoLevel)
			do(mount(h), http.MethodPost, tc.path, tc.body)
			if strings.Contains(logged.String(), secret) {
				t.Errorf("the question reached the log: %s", logged.String())
			}
		})
	}

	// And the writer is live, or every case above passes because nothing logs
	// at all.
	var logged bytes.Buffer
	h := hermeticHandler(newStore(), &fakeRetriever{err: errors.New("connection refused")})
	h.Log = zerolog.New(&logged).Level(zerolog.InfoLevel)
	do(mount(h), http.MethodPost, "/api/repos/repo-1/ask", `{"q":"`+secret+`"}`)
	if !strings.Contains(logged.String(), "connection refused") {
		t.Fatalf("nothing was logged for a 500, so this test proves nothing: %q", logged.String())
	}
}

// A lexical-only run has no cosine similarity. json.Marshal fails outright on
// NaN — the response would be a 500 with a half-written body — so null is what
// travels, and the floor says it does not apply.
func TestANaNTopScoreSerialisesAsNull(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{res: result(rag.ModeLexical, math.NaN(), false)}
	for _, route := range []string{"search", "ask"} {
		rec := do(mount(hermeticHandler(st, rt)), http.MethodPost,
			"/api/repos/repo-1/"+route, `{"q":"sampler"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d: %s", route, rec.Code, rec.Body)
		}
		out := body(t, rec)
		v, ok := out["top_score"]
		if !ok || v != nil {
			t.Errorf("%s: top_score %v, want null", route, v)
		}
		if route == "ask" {
			floor, _ := out["floor"].(map[string]any)
			// No cosine similarity means no floor to compare against;
			// ts_rank_cd is unbounded and corpus-dependent.
			if floor["applicable"] != false {
				t.Errorf("the floor claims to apply in lexical mode: %v", floor)
			}
			if out["refused"] != false {
				t.Errorf("lexical-only refused: %s", rec.Body)
			}
		}
	}
}

func TestTheCorpusListingIsNewestUsedFirstAndBounded(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{}
	older := models.Repo{ID: "repo-0", Remote: "https://codeberg.org/a/b", Ref: "main",
		Commit: "beef", IndexedAt: fixtureIndexedAt.Add(-time.Hour)}
	st.rows = []store.RepoRow{
		{Repo: fixtureRepo, LastUsedAt: fixtureNow},
		{Repo: older, LastUsedAt: fixtureNow.Add(-time.Hour)},
	}
	rec := do(mount(hermeticHandler(st, rt)), http.MethodGet, "/api/repos?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Count int `json:"count"`
		Repos []struct {
			ID         string    `json:"id"`
			Remote     string    `json:"remote"`
			Commit     string    `json:"commit"`
			LastUsedAt time.Time `json:"last_used_at"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 || len(out.Repos) != 2 {
		t.Fatalf("count %d: %s", out.Count, rec.Body)
	}
	if out.Repos[0].ID != fixtureRepoID || out.Repos[1].ID != "repo-0" {
		t.Errorf("order %s, %s", out.Repos[0].ID, out.Repos[1].ID)
	}
	// last_used_at is what explains why a repository a caller had a link to is
	// gone, so it is on the wire and it is the clock, not indexed_at.
	if !out.Repos[0].LastUsedAt.Equal(fixtureNow) {
		t.Errorf("last_used_at %v, want %v", out.Repos[0].LastUsedAt, fixtureNow)
	}
	if out.Repos[0].Commit != fixtureCommit {
		t.Errorf("commit %q", out.Repos[0].Commit)
	}
}

func TestTheRepoViewReportsCoverageAsThreeNumbersAndItsStaleness(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{}
	st.newer = rag.Newer{Commit: "9999999abcdef", IndexedAt: fixtureNow.Add(-time.Hour)}
	rec := do(mount(hermeticHandler(st, rt)), http.MethodGet, "/api/repos/repo-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	out := body(t, rec)
	for k, want := range map[string]float64{"files": 12, "spans": 30, "files_with_spans": 9} {
		if out[k] != want {
			t.Errorf("%s = %v, want %v", k, out[k], want)
		}
	}
	if out["commit"] != fixtureCommit || out["ref"] != "main" {
		t.Errorf("body %s", rec.Body)
	}
	stale, _ := out["staleness"].(map[string]any)
	// The one positive claim the corpus supports: this ref is also indexed here
	// at a later commit, so it demonstrably moved.
	if stale["state"] != rag.StaleSuperseded || stale["newer_commit"] != "9999999abcdef" {
		t.Errorf("staleness %v", stale)
	}
	if stale["forge_checked"] != false {
		t.Errorf("the repo view claims the forge was asked: %v", stale)
	}
}

func TestASpanIsServedWithItsCitationAndWithoutItsJoinKeys(t *testing.T) {
	st, rt := newStore(), &fakeRetriever{}
	rec := do(mount(hermeticHandler(st, rt)), http.MethodGet, "/api/repos/repo-1/spans/span-c", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Span     map[string]any `json:"span"`
		Citation rag.Citation   `json:"citation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	s := spanByID("span-c")
	if out.Span["text"] != s.Text || out.Span["start_line"] != float64(s.StartLine) {
		t.Errorf("span %v", out.Span)
	}
	if out.Citation.Digest != s.Digest || out.Citation.EndLine != s.EndLine {
		t.Errorf("citation %+v", out.Citation)
	}
	// file_id and repo_id are join keys; the citation next to this carries the
	// repository.
	for _, internal := range []string{"file_id", "repo_id"} {
		if _, ok := out.Span[internal]; ok {
			t.Errorf("the span view serves %q: %s", internal, rec.Body)
		}
	}

	// A span of another repository is a 404 for this one, not a read across a
	// corpus boundary the caller never named.
	if rec := do(mount(hermeticHandler(st, rt)), http.MethodGet,
		"/api/repos/repo-1/spans/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown span: want 404, got %d: %s", rec.Code, rec.Body)
	}
}

// counters reads the process's own metrics the way Prometheus does, so what is
// asserted is what an operator sees.
func counters(t *testing.T) map[string]float64 {
	t.Helper()
	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "codetrail_answer_total{") &&
			!strings.HasPrefix(line, "codetrail_refusal_total{") {
			continue
		}
		series, value, ok := strings.Cut(line, "} ")
		if !ok {
			t.Fatalf("unparseable metric line %q", line)
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("metric %q: %v", line, err)
		}
		out[series+"}"] = v
	}
	if len(out) == 0 {
		t.Fatal("no answer or refusal counters are registered; the assertions below would prove nothing")
	}
	return out
}

// movedSince returns only the series that changed, so an assertion names the
// whole movement rather than the one series it expected.
func movedSince(t *testing.T, before map[string]float64) map[string]float64 {
	t.Helper()
	moved := map[string]float64{}
	for series, now := range counters(t) {
		if d := now - before[series]; d != 0 {
			moved[series] = d
		}
	}
	return moved
}
