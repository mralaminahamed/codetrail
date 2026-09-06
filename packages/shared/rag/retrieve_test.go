package rag

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

const fixtureDim = 16

// cite is one row as an arm would return it. Score is a float32 in the column
// and in models.Cite, so every score here is chosen to be exactly
// representable in one: 0.83 arrives as 0.8299999833106995 and no exact
// assertion against it can be written.
func cite(id, path string, line int, score float32) models.Cite {
	return models.Cite{
		Span: models.Span{
			ID: id, Path: path, StartLine: line,
			Text: "text of " + id, Digest: "digest-" + id,
		},
		Score: score,
	}
}

func cites(ids ...string) []models.Cite {
	out := make([]models.Cite, len(ids))
	for i, id := range ids {
		// Descending, so "the arm's best" is unambiguous, and non-uniform, so
		// no two spans tie.
		out[i] = cite(id, id+".go", 1, 0.75-float32(i)/64)
	}
	return out
}

type armCall struct {
	repoID string
	limit  int
	q      []float32
	terms  []string
}

// fakeSearcher is what makes arm behaviour controllable and, more to the
// point, what makes "this arm did not run" observable: an arm that ran and
// returned nothing is indistinguishable from an arm that never ran, in the
// result.
//
// It truncates to the limit it was asked for, as a real arm does. Without
// that, asking for the wrong depth would change nothing a test could see.
type fakeSearcher struct {
	vector, lexical []models.Cite
	model           string
	dim             int
	err             error

	vecCalls, lexCalls, embedderCalls []armCall
}

func (f *fakeSearcher) VectorSearch(_ context.Context, repoID string, q []float32, limit int) ([]models.Cite, error) {
	f.vecCalls = append(f.vecCalls, armCall{repoID: repoID, limit: limit, q: q})
	if f.err != nil {
		return nil, f.err
	}
	return f.vector[:min(limit, len(f.vector))], nil
}

func (f *fakeSearcher) LexicalSearch(_ context.Context, repoID string, terms []string, limit int) ([]models.Cite, error) {
	f.lexCalls = append(f.lexCalls, armCall{repoID: repoID, limit: limit, terms: terms})
	return f.lexical[:min(limit, len(f.lexical))], nil
}

func (f *fakeSearcher) SpanEmbedder(_ context.Context, repoID string) (string, int, error) {
	f.embedderCalls = append(f.embedderCalls, armCall{repoID: repoID})
	return f.model, f.dim, nil
}

// countingEmbedder is the only way to see an embed call that nothing reads:
// the results are identical whether or not the query was embedded in lexical
// mode, and only the model's bill would show it.
type countingEmbedder struct {
	embed.Embedder
	n int
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.n++
	return c.Embedder.Embed(ctx, texts)
}

// The fixture: the arms return different orders over overlapping spans, so a
// retriever that returns one arm verbatim, or that runs the wrong arm, is
// visible in the order. Vector is [A B C D], lexical is [C A E]; fused at
// k=60 that is [A C B E D] (Task 1 pins the arithmetic).
func retriever(t *testing.T, mode Mode) (*Retriever, *fakeSearcher, *countingEmbedder) {
	t.Helper()
	emb := &countingEmbedder{Embedder: embed.NewFake(fixtureDim)}
	st := &fakeSearcher{
		vector:  cites("A", "B", "C", "D"),
		lexical: cites("C", "A", "E"),
		model:   emb.Model(),
		dim:     fixtureDim,
	}
	return &Retriever{Store: st, Emb: emb, Mode: mode, Fusion: DefaultParams(), Candidates: 40, Split: true}, st, emb
}

func ranked(r Result) []string {
	out := make([]string, len(r.Hits))
	for i, h := range r.Hits {
		out[i] = h.SpanID
	}
	return out
}

func search(t *testing.T, r *Retriever, limit int) Result {
	t.Helper()
	out, err := r.Search(context.Background(), "repo-1", "parseConfig handler", limit)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return out
}

func TestModeVectorDoesNotRunTheLexicalArm(t *testing.T) {
	r, st, _ := retriever(t, ModeVector)
	got := search(t, r, 10)
	if len(st.lexCalls) != 0 {
		t.Fatalf("lexical arm was queried in vector mode: %+v", st.lexCalls)
	}
	if len(st.vecCalls) != 1 {
		t.Fatalf("vector arm ran %d times, want 1", len(st.vecCalls))
	}
	// The vector arm's own order, and E — lexical-only — nowhere in it.
	if want := []string{"A", "B", "C", "D"}; !slices.Equal(ranked(got), want) {
		t.Fatalf("got %v, want %v", ranked(got), want)
	}
	if !got.VectorRan || got.Mode != ModeVector {
		t.Fatalf("vectorRan=%v mode=%s", got.VectorRan, got.Mode)
	}
}

func TestModeLexicalDoesNotRunTheVectorArmOrEmbedTheQuery(t *testing.T) {
	r, st, emb := retriever(t, ModeLexical)
	got := search(t, r, 10)
	if len(st.vecCalls) != 0 {
		t.Fatalf("vector arm was queried in lexical mode: %+v", st.vecCalls)
	}
	if emb.n != 0 {
		t.Fatalf("embedder called %d times in lexical mode", emb.n)
	}
	// The model guard is about two vector spaces. There is no vector here, so
	// asking which model wrote the corpus is a round trip for nothing.
	if len(st.embedderCalls) != 0 {
		t.Fatalf("the corpus embedder was read in lexical mode: %+v", st.embedderCalls)
	}
	if want := []string{"C", "A", "E"}; !slices.Equal(ranked(got), want) {
		t.Fatalf("got %v, want %v", ranked(got), want)
	}
	if got.VectorRan {
		t.Fatal("a lexical-only result claims the vector arm ran")
	}
}

func TestModeHybridRunsBoth(t *testing.T) {
	r, st, emb := retriever(t, ModeHybrid)
	got := search(t, r, 10)
	if len(st.vecCalls) != 1 || len(st.lexCalls) != 1 {
		t.Fatalf("vector ran %d times, lexical %d, want 1 each", len(st.vecCalls), len(st.lexCalls))
	}
	if emb.n != 1 {
		t.Fatalf("the query was embedded %d times, want once", emb.n)
	}
	// Neither arm's own order, which is what makes this able to fail: [A B C D]
	// and [C A E] both pass a test that accepts either arm verbatim.
	want := []string{"A", "C", "B", "E", "D"}
	if !slices.Equal(ranked(got), want) {
		t.Fatalf("got %v, want %v", ranked(got), want)
	}
	if got.Hits[0].VectorRank != 1 || got.Hits[0].LexicalRank != 2 {
		t.Fatalf("A has ranks v=%d l=%d, want v=1 l=2", got.Hits[0].VectorRank, got.Hits[0].LexicalRank)
	}
	// Every arm is asked about the repo the caller named, not some other one.
	if st.vecCalls[0].repoID != "repo-1" || st.lexCalls[0].repoID != "repo-1" {
		t.Fatalf("arms queried %q and %q", st.vecCalls[0].repoID, st.lexCalls[0].repoID)
	}
	// The query reaches the vector arm as a vector of the corpus's width.
	if len(st.vecCalls[0].q) != fixtureDim {
		t.Fatalf("the vector arm was given a %d-wide query, want %d", len(st.vecCalls[0].q), fixtureDim)
	}
}

// The floor reads this number, so it has to be the vector arm's own
// similarity. Measured under a mutant that read the fused score instead: this
// fixture's fused top is 0.0325224 — 1/61 + 1/62, a function of the ranks and
// of nothing else — which looks like a plausible small similarity and says
// nothing about quality.
func TestTopScoreIsTheVectorArmsBestSimilarity(t *testing.T) {
	r, _, _ := retriever(t, ModeHybrid)
	got := search(t, r, 10)
	if got.TopScore != 0.75 {
		t.Fatalf("TopScore %v, want 0.75", got.TopScore)
	}
	// And it is not the fused score, which is what a floor must never read.
	if got.TopScore == got.Hits[0].Score {
		t.Fatalf("TopScore is the fused score %v", got.Hits[0].Score)
	}
	// The number survives onto the hit the floor is about.
	if got.Hits[0].VectorScore != 0.75 {
		t.Fatalf("the top hit carries VectorScore %v, want 0.75", got.Hits[0].VectorScore)
	}
}

// In lexical-only mode there is no cosine similarity at all. VectorRan is what
// tells Decide not to compare a cosine floor against a ts_rank.
func TestLexicalOnlyResultsAreNotScoredForTheFloor(t *testing.T) {
	r, _, _ := retriever(t, ModeLexical)
	got := search(t, r, 10)
	if !math.IsNaN(got.TopScore) {
		t.Fatalf("a lexical-only run scored %v; a ts_rank in a cosine field is a category error", got.TopScore)
	}
	if out, why := Decide(Floor{Value: 0.9}, len(got.Hits), got.TopScore, got.VectorRan); out != OutcomeAnswered {
		t.Fatalf("a lexical-only result was refused by a cosine floor: %s/%s", out, why)
	}
}

// An empty vector arm has no best similarity either, and NaN is the honest
// encoding: Decide refuses on it, and the histogram drops it.
func TestAnEmptyVectorArmHasNoTopScore(t *testing.T) {
	r, st, _ := retriever(t, ModeHybrid)
	st.vector = nil
	got := search(t, r, 10)
	if !math.IsNaN(got.TopScore) {
		t.Fatalf("TopScore %v with nothing retrieved by the vector arm", got.TopScore)
	}
	if !got.VectorRan {
		t.Fatal("the vector arm ran and returned nothing; that is not the same as not running")
	}
}

// A corpus embedded by another model is a misconfiguration, not a low score:
// the query and the corpus are in different vector spaces, every score is
// meaningless rather than small, and nothing about the query looks wrong.
func TestARepoIndexedByAnotherModelIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		dim         int
	}{
		{"another model", "some-other-model", fixtureDim},
		// The fake names itself the same at every width, so the width is not
		// redundant with the name.
		{"the same model at another width", "fake-hashed-bow", fixtureDim * 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, _ := retriever(t, ModeHybrid)
			st.model, st.dim = tc.model, tc.dim
			out, err := r.Search(context.Background(), "repo-1", "parseConfig handler", 10)
			if !errors.Is(err, ErrModelMismatch) {
				t.Fatalf("got %d hits and error %v, want ErrModelMismatch", len(out.Hits), err)
			}
			if !strings.Contains(err.Error(), tc.model) {
				t.Fatalf("the error does not name what the corpus was indexed with: %v", err)
			}
			if len(st.vecCalls) != 0 {
				t.Fatal("the arms ran anyway")
			}
			// The failed result still carries what was configured to run. A
			// caller that reads an error as a caller-facing outcome — the
			// gateway does, for a repo with no spans — reports the mode it
			// retrieved in and a top score of null from this, and a zero
			// Result would give it mode "" and a similarity of 0.
			if out.Mode != ModeHybrid || !out.VectorRan || !math.IsNaN(out.TopScore) {
				t.Errorf("a failed retrieval returned %+v", out)
			}
		})
	}
}

// Candidate depth is per arm and the limit applies after fusion: fusing two
// lists of `limit` is mostly an intersection test.
//
// deep is 30th in the vector arm and 1st in the lexical one, so it is only
// reachable at depth: at a depth of 10 the vector arm never returns it, and
// its vector rank — the thing that pushes it to the top — is lost.
func TestArmsAreQueriedAtCandidateDepthAndTrimmedAfterFusion(t *testing.T) {
	r, st, _ := retriever(t, ModeHybrid)
	st.vector = cites("v01", "v02", "v03", "v04", "v05", "v06", "v07", "v08", "v09", "v10",
		"v11", "v12", "v13", "v14", "v15", "v16", "v17", "v18", "v19", "v20",
		"v21", "v22", "v23", "v24", "v25", "v26", "v27", "v28", "v29", "deep")
	st.lexical = cites("deep", "l02", "l03")

	got := search(t, r, 10)
	if st.vecCalls[0].limit != 40 || st.lexCalls[0].limit != 40 {
		t.Fatalf("vector arm asked for %d candidates and lexical for %d, want 40",
			st.vecCalls[0].limit, st.lexCalls[0].limit)
	}
	if got.Hits[0].SpanID != "deep" || got.Hits[0].VectorRank != 30 {
		t.Fatalf("first hit is %q at vector rank %d, want deep at 30",
			got.Hits[0].SpanID, got.Hits[0].VectorRank)
	}
	// Trimmed after fusion, not before: 32 spans came back and 10 leave.
	if len(got.Hits) != 10 {
		t.Fatalf("returned %d hits for a limit of 10", len(got.Hits))
	}
	// And the spans a caller renders citations from are exactly those hits.
	if len(got.Spans) != 10 {
		t.Fatalf("carried %d spans for 10 hits", len(got.Spans))
	}
	for _, h := range got.Hits {
		if got.Spans[h.SpanID].Text != "text of "+h.SpanID {
			t.Fatalf("no span text for %s", h.SpanID)
		}
	}
}

// The lexical arm is asked for the query's terms, not the query. Handing a
// question about code to to_tsquery raw is a syntax error for any of & | ! : (
// and the split is a knob P6 sweeps, so both have to reach the arm.
func TestTheLexicalArmIsAskedForTheQuerysTerms(t *testing.T) {
	for _, tc := range []struct {
		split bool
		want  []string
	}{
		{true, []string{"parseconfig", "parse", "config", "handler"}},
		{false, []string{"parseconfig", "handler"}},
	} {
		r, st, _ := retriever(t, ModeLexical)
		r.Split = tc.split
		search(t, r, 10)
		if !slices.Equal(st.lexCalls[0].terms, tc.want) {
			t.Fatalf("split=%v: terms %q, want %q", tc.split, st.lexCalls[0].terms, tc.want)
		}
	}
}

// An arm that fails is an error, not an empty result: an empty result is a
// refusal, and "the database is down" must never be filed as "we had nothing
// to say" (spec §10).
func TestAnArmErrorIsAnErrorAndNotAnEmptyResult(t *testing.T) {
	r, st, _ := retriever(t, ModeHybrid)
	st.err = errors.New("connection refused")
	out, err := r.Search(context.Background(), "repo-1", "parseConfig handler", 10)
	if err == nil {
		t.Fatalf("a failing arm returned %d hits and no error", len(out.Hits))
	}
}

// Fuse has no guard on k and the retriever has no guard on the limit; both are
// validated where they are configured. This is the second line: a Retriever
// constructed in code (the eval does exactly that, spec §9) must not fuse with
// a negative k, where k=-1 divides by zero and k<=-2 inverts the ranking.
func TestSearchRefusesAConfigurationFuseCannotSurvive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		k, cand, l int
	}{
		{"negative k", -1, 40, 10},
		{"zero limit", 60, 40, 0},
		{"zero candidates", 60, 0, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := retriever(t, ModeHybrid)
			r.Fusion.K, r.Candidates = tc.k, tc.cand
			if _, err := r.Search(context.Background(), "repo-1", "q", tc.l); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// scrape reads the process's own metrics the way Prometheus does, so the
// assertions below are about what an operator sees rather than about an
// internal counter.
func scrape(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// figures reads the retrieval metrics out of a scrape and parses their values,
// so the assertions below are on the movement an operator would see.
//
// The movement and never the value: promauto registers every series at init, so
// a histogram nobody observed scrapes as _sum 0 / _count 0 — indistinguishable
// from one whose observations happen to sum to zero, and equally free of NaN.
// A deleted observation is only visible as a delta that did not happen.
func figures(t *testing.T) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, line := range strings.Split(scrape(t), "\n") {
		if !strings.HasPrefix(line, "codetrail_retrieval_") {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("unparseable metric line %q", line)
		}
		out[name] = v
	}
	if len(out) == 0 {
		t.Fatal("no retrieval metrics are registered; the assertions here would prove nothing")
	}
	return out
}

const (
	topScoreSum   = "codetrail_retrieval_top_score_sum"
	topScoreCount = "codetrail_retrieval_top_score_count"
)

// The instrument the floor is calibrated from in P6, pinned to an observation
// rather than to the absence of one. This is the histogram that carries the
// argument for shipping an uncalibrated floor — that P3 ships the mechanism and
// the instrument — so the observation must not be deletable with the suite
// green: the count moves by exactly one retrieval, and the sum moves by the
// score that retrieval reported.
//
// Latency is asserted the same way and for the same reason, one-sided in the
// other direction: it is the retriever that knows what "retrieval latency"
// covers, and it is labelled by the mode that ran.
func TestTheTopScoreHistogramObservesTheScoreThatWasRetrieved(t *testing.T) {
	r, _, _ := retriever(t, ModeHybrid)
	before := figures(t)
	got := search(t, r, 10)
	after := figures(t)

	// Restated where the delta is read: the sum below means nothing unless the
	// number the retrieval reported is known here.
	if got.TopScore != 0.75 {
		t.Fatalf("fixture: the vector arm's best is %v, want 0.75", got.TopScore)
	}
	if n := after[topScoreCount] - before[topScoreCount]; n != 1 {
		t.Fatalf("the top-score histogram took %v observations for one retrieval, want 1", n)
	}
	// The score itself, not merely that something was observed: a histogram
	// handed the fused score, or a constant, moves the count identically.
	if sum := after[topScoreSum] - before[topScoreSum]; math.Abs(sum-got.TopScore) > 1e-9 {
		t.Fatalf("the histogram's sum moved by %v for a retrieval that scored %v", sum, got.TopScore)
	}
	const timed = `codetrail_retrieval_seconds_count{mode="hybrid"}`
	if n := after[timed] - before[timed]; n != 1 {
		t.Fatalf("one hybrid retrieval was timed %v times under its own mode", n)
	}
}

// A lexical-only run has no cosine similarity at all, and observing its NaN
// would make the histogram's _sum NaN for the life of the process — every
// quantile and every average over it, permanently, with no error anywhere.
//
// So this run must move the latency histogram and leave the top-score one
// exactly where it was. Both halves are deltas: "the sum is not NaN" is also
// true of a histogram that was never observed, which is what let the
// observation above be deleted with this suite green.
func TestALexicalOnlyRunIsTimedAndObservesNoTopScore(t *testing.T) {
	r, _, _ := retriever(t, ModeLexical)
	before := figures(t)
	search(t, r, 10)
	after := figures(t)

	if math.IsNaN(after[topScoreSum]) {
		t.Fatalf("the top-score histogram is poisoned: %s is NaN", topScoreSum)
	}
	if n := after[topScoreCount] - before[topScoreCount]; n != 0 {
		t.Fatalf("a lexical-only run put %v observations in the top-score histogram", n)
	}
	const timed = `codetrail_retrieval_seconds_count{mode="lexical"}`
	if n := after[timed] - before[timed]; n != 1 {
		t.Fatalf("one lexical retrieval was timed %v times under its own mode", n)
	}
}
