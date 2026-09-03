package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/corpus"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// searcher is a rag.Searcher whose two arms return exactly what a fixture
// says. The retriever under test is the shipped one; only the store is faked.
type searcher struct {
	vector  []models.Cite
	lexical []models.Cite
	err     error
}

func (s *searcher) VectorSearch(_ context.Context, _ string, _ []float32, limit int) ([]models.Cite, error) {
	return trim(s.vector, limit), s.err
}

func (s *searcher) LexicalSearch(_ context.Context, _ string, _ []string, limit int) ([]models.Cite, error) {
	return trim(s.lexical, limit), s.err
}

func (s *searcher) SpanEmbedder(context.Context, string) (string, int, error) {
	return FakeModel, store.EmbeddingDim, s.err
}

func trim(cs []models.Cite, limit int) []models.Cite {
	if len(cs) > limit {
		return cs[:limit]
	}
	return cs
}

// cite builds one ranked row: a span in one file at one range, with the arm's
// own score on it.
func cite(id string, start, end int, score float32) models.Cite {
	return models.Cite{
		Span: models.Span{
			ID: id, Path: "store.go", Kind: models.KindFunc, Symbol: id,
			StartLine: start, EndLine: end,
			Text: "func " + id + "() {}", Digest: store.Digest(id),
		},
		Score: score,
	}
}

// ladder is ten spans at descending cosine, in ten disjoint ten-line bands. The
// case under test claims lines 61-69, so its gold span is s7 — rank 7, past
// the shipped MaxSpans of 5 and inside a limit of 10.
func ladder() ([]models.Cite, []metric.Span) {
	var cs []models.Cite
	var spans []metric.Span
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("s%d", i)
		start, end := (i-1)*10+1, (i-1)*10+9
		cs = append(cs, cite(id, start, end, float32(0.72)-float32(i-1)*0.01))
		spans = append(spans, metric.Span{ID: id, Path: "store.go", Start: start, End: end})
	}
	return cs, spans
}

func arm(t *testing.T, name string, mode rag.Mode, s *searcher, spans []metric.Span) Arm {
	t.Helper()
	return Arm{
		Name:   name,
		Corpus: corpus.Arm{Name: name, DSN: "postgres://h/" + name, RepoID: "r"},
		Retriever: &rag.Retriever{
			Store: s, Emb: embed.NewFake(store.EmbeddingDim), Mode: mode,
			Fusion: rag.DefaultParams(), Candidates: 40, Split: true, Floor: rag.DefaultFloor(),
		},
		Repo:  models.Repo{ID: "r", Remote: "https://github.com/eval/repo", Ref: "main", Commit: "c0ffee"},
		Spans: spans,
		Stats: ArmStats{Spans: len(spans), Files: 1, FilesWithSpans: 1},
	}
}

func caseAt(id string, start, end int) golden.Case {
	return golden.Case{
		ID: id, Path: "store.go", Symbol: id, Kind: models.KindFunc, DocOf: golden.DocOfDecl,
		Question: "The thing " + id + " does, in prose.\n",
		Start:    start, End: end, RawStart: start - 2, RawEnd: end,
	}
}

func cfg() Config {
	return Config{
		Mode: rag.ModeHybrid, Fusion: rag.DefaultParams(), Candidates: 40, Split: true, Emb: embed.NewFake(store.EmbeddingDim),
		Ks: []int{1, 5, 10}, Limit: 10, Floor: rag.DefaultFloor(), Budget: rag.DefaultBudget(),
		Floors: []float64{0.5}, Now: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
	}
}

func runOne(t *testing.T, a Arm, cs []golden.Case, c Config) ArmResult {
	t.Helper()
	res, err := RunArm(context.Background(), a, cs, c)
	if err != nil {
		t.Fatalf("RunArm: %v", err)
	}
	return res
}

// hit@10 over a list of 5 is hit@5 wearing a different label, and nothing in
// the output would say so. The gold span here is at rank 7 on purpose: a
// fixture whose gold always ranks in the top five cannot tell the two apart.
func TestTheRunnerRetrievesToTheDeepestKAndNotToTheAnswerBudget(t *testing.T) {
	cs, spans := ladder()
	res := runOne(t, arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs}, spans),
		[]golden.Case{caseAt("c", 61, 69)}, cfg())

	r := res.Cases[0]
	if r.GoldRank != 7 {
		t.Errorf("gold rank is %d, want 7; ranked %v", r.GoldRank, r.Ranked)
	}
	if r.RankedLen != 10 || r.Limit != 10 {
		t.Errorf("retrieved %d spans at limit %d, want 10 and 10", r.RankedLen, r.Limit)
	}
	want := []HitRate{{K: 1, Lenient: 0, Strict: 0}, {K: 5, Lenient: 0, Strict: 0}, {K: 10, Lenient: 1, Strict: 1}}
	if !reflect.DeepEqual(res.Summary.HitAt, want) {
		t.Errorf("hit rates are %+v, want %+v", res.Summary.HitAt, want)
	}
	if res.Summary.MRR != 1.0/7.0 {
		t.Errorf("MRR is %v, want %v", res.Summary.MRR, 1.0/7.0)
	}
}

// rag.Assemble stops at MaxSpans=5, so a gold span ranked seventh is retrieved
// and never shown. "Answered without citing the right span" is the outcome a
// user experiences, and scoring the ranked list instead reports a system
// better than the one that ships.
func TestAGoldSpanRankedPastTheBudgetIsRetrievedAndNotCited(t *testing.T) {
	cs, spans := ladder()
	c := cfg()
	res := runOne(t, arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs}, spans),
		[]golden.Case{caseAt("c", 61, 69)}, c)
	r := res.Cases[0]
	if c.Budget.MaxSpans != 5 {
		t.Fatalf("the shipped budget is %d spans; this fixture needs 5", c.Budget.MaxSpans)
	}
	if r.AnswerHasGold {
		t.Errorf("answer_has_gold is true for a gold span at rank 7 under MaxSpans=%d; want false", c.Budget.MaxSpans)
	}
	if r.AnswerCitations != 5 {
		t.Errorf("the answer cited %d spans, want 5", r.AnswerCitations)
	}
	if r.GoldRank != 7 {
		t.Errorf("gold rank %d; the case must still be a hit at 10", r.GoldRank)
	}
	// The confusion at a floor the cosine clears: answered, and without gold.
	got := metric.Sweep([]metric.Outcome{r.Outcomes()}, []float64{0.5})[0]
	if got.AnsweredWithoutGold != 1 || got.AnsweredWithGold != 0 {
		t.Errorf("confusion is %+v; want the case answered without gold", got)
	}
}

// Where the phase would silently go wrong. The vector cosine here is 0.72 and
// the fused top score is a sum of reciprocals near 1/k — two orders of
// magnitude apart, which is the ordinary case and not a contrived one. At a
// floor of 0.5 the correct implementation answers and a sweep fed the fused
// score refuses everything.
func TestTheFloorSweepReadsTheVectorCosineAndNotTheFusedScore(t *testing.T) {
	cs, spans := ladder()
	cases := []golden.Case{caseAt("a", 1, 9), caseAt("b", 11, 19), caseAt("c", 21, 29), caseAt("d", 91, 99)}
	res := runOne(t, arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs, lexical: cs[:3]}, spans), cases, cfg())

	for _, r := range res.Cases {
		if r.TopScore == nil || *r.TopScore != 0.7200000286102295 {
			t.Errorf("case %s top score is %v, want the vector arm's cosine", r.CaseID, r.TopScore)
		}
		if r.FusedTop == nil || *r.FusedTop >= 0.1 {
			t.Errorf("case %s fused top is %v; an RRF score is a function of ranks alone and sits near 1/k", r.CaseID, r.FusedTop)
		}
	}
	got := metric.Sweep(outcomes(res), []float64{0.5})[0]
	want := metric.Confusion{
		Floor: 0.5, AnsweredWithGold: 3, AnsweredWithoutGold: 1,
		ByReason: map[rag.Reason]int{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("confusion at floor 0.5 is %+v, want %+v", got, want)
	}
}

func TestACaseWithNoGoldSpanInThisArmIsUnscoreableAndNotAMiss(t *testing.T) {
	cs, spans := ladder()
	cases := []golden.Case{
		caseAt("a", 1, 9),
		// Lines no span of this arm covers, and a different file besides.
		{ID: "elsewhere", Path: "other.go", Symbol: "E", Kind: models.KindFunc,
			DocOf: golden.DocOfDecl, Question: "Prose about E.\n", Start: 1, End: 9, RawStart: 1, RawEnd: 9},
	}
	res := runOne(t, arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs}, spans), cases, cfg())

	byID := map[string]CaseRecord{}
	for _, r := range res.Cases {
		byID[r.CaseID] = r
	}
	if byID["elsewhere"].Scoreable {
		t.Error("a case with no gold span in this arm is scoreable")
	}
	if res.Summary.Unscoreable != 1 {
		t.Errorf("Unscoreable is %d, want 1", res.Summary.Unscoreable)
	}
	// Out of every denominator: with one scoreable case hitting at rank 1, MRR
	// is 1 and not 1/2.
	if res.Summary.MRR != 1 {
		t.Errorf("MRR is %v, want 1; the unscoreable case is in no denominator", res.Summary.MRR)
	}
	for _, h := range res.Summary.HitAt {
		if h.Lenient != 1 {
			t.Errorf("hit@%d is %v, want 1", h.K, h.Lenient)
		}
	}
	if got := metric.Sweep(outcomes(res), []float64{0.5})[0]; got.Unscoreable != 1 {
		t.Errorf("confusion is %+v; the unscoreable case has its own cell", got)
	}
}

// §9 asks CI for a deterministic run, and Go randomises map iteration on
// purpose. Two runs in one process, because a single run cannot discriminate.
func TestTheRecordedOrderIsStableAcrossRuns(t *testing.T) {
	cs, spans := ladder()
	cases := []golden.Case{
		caseAt("d", 91, 99), caseAt("a", 1, 9), caseAt("c", 21, 29), caseAt("b", 11, 19),
		caseAt("f", 51, 59), caseAt("e", 41, 49),
	}
	a := arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs}, spans)
	one := ids(runOne(t, a, cases, cfg()))
	two := ids(runOne(t, a, cases, cfg()))
	if !reflect.DeepEqual(one, two) {
		for i := range one {
			if i < len(two) && one[i] != two[i] {
				t.Fatalf("run 2 differs from run 1 at case %d: %q != %q", i, two[i], one[i])
			}
		}
		t.Fatalf("run 1 recorded %v and run 2 recorded %v", one, two)
	}
	// And the order is the sort, not the input: an input already in the
	// expected order could not tell the two apart.
	want := []string{"a", "b", "c", "e", "f", "d"}
	if !reflect.DeepEqual(one, want) {
		t.Errorf("recorded order is %v, want %v (path, start, symbol, docOf)", one, want)
	}
	if reflect.DeepEqual(one, []string{"d", "a", "c", "b", "f", "e"}) {
		t.Error("the recorded order is the input order and cannot discriminate")
	}
}

func TestBothArmsGetTheSameQuestionSet(t *testing.T) {
	cs, spans := ladder()
	cases := []golden.Case{caseAt("b", 11, 19), caseAt("a", 1, 9), caseAt("c", 21, 29)}
	// Two arms over two different corpora, as the phase runs them.
	ast := arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs}, spans)
	win := arm(t, "window", rag.ModeHybrid, &searcher{vector: cs[:4]}, spans[:4])
	a, b := ids(runOne(t, ast, cases, cfg())), ids(runOne(t, win, cases, cfg()))
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the arms were asked %v and %v", a, b)
	}
	if len(a) != 3 {
		t.Errorf("each arm saw %d cases, want 3", len(a))
	}
}

// In lexical mode there is no cosine at all, so the floor does not apply — a
// different sentence from "the floor refused nothing", and the artefact has to
// say which.
func TestALexicalRunReportsTheFloorAsInapplicable(t *testing.T) {
	cs, spans := ladder()
	a := arm(t, "lex", rag.ModeLexical, &searcher{lexical: cs}, spans)
	res := runOne(t, a, []golden.Case{caseAt("a", 1, 9)}, cfg())
	if res.Summary.FloorApplicable {
		t.Error("a lexical run reports its floor as applicable")
	}
	r := res.Cases[0]
	if r.VectorRan || r.TopScore != nil {
		t.Errorf("the lexical run recorded vector_ran=%v top_score=%v", r.VectorRan, r.TopScore)
	}
	if r.Outcome != rag.OutcomeAnswered || r.Reason != "" {
		t.Errorf("the lexical case decided (%s, %s), want answered", r.Outcome, r.Reason)
	}
	// And a hybrid run over the same fixture does apply it.
	h := arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs}, spans)
	if !runOne(t, h, []golden.Case{caseAt("a", 1, 9)}, cfg()).Summary.FloorApplicable {
		t.Error("a hybrid run reports its floor as inapplicable")
	}
}

func TestARecordCarriesTheGoldSpansRankInEachArm(t *testing.T) {
	cs, spans := ladder()
	// The gold span is rank 3 in the vector arm and rank 1 in the lexical one,
	// which is the shape spec:316's question is about.
	lex := []models.Cite{cs[2], cs[0], cs[1]}
	res := runOne(t, arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs, lexical: lex}, spans),
		[]golden.Case{caseAt("c", 21, 29)}, cfg())
	r := res.Cases[0]
	if r.GoldVectorRank != 3 || r.GoldLexicalRank != 1 {
		t.Errorf("gold ranks are vector %d lexical %d, want 3 and 1", r.GoldVectorRank, r.GoldLexicalRank)
	}
	if res.Summary.GoldFoundByBoth != 1 {
		t.Errorf("gold-found-by counts are %+v; want one found by both", res.Summary)
	}
}

func TestAnUnscoredHybridResultIsRefusedAndRecordedAsSuch(t *testing.T) {
	// No rows from either arm: Decide refuses no_spans, and the record says so
	// rather than reporting a top score of zero, which is a real cosine.
	res := runOne(t, arm(t, "ast", rag.ModeHybrid, &searcher{}, []metric.Span{
		{ID: "s1", Path: "store.go", Start: 1, End: 9},
	}), []golden.Case{caseAt("a", 1, 9)}, cfg())
	r := res.Cases[0]
	if r.Outcome != rag.OutcomeRefused || r.Reason != rag.ReasonNoSpans {
		t.Errorf("an empty result decided (%s, %s), want (refused, no_spans)", r.Outcome, r.Reason)
	}
	if r.TopScore != nil {
		t.Errorf("top score is %v, want null: NaN is not zero and zero is a real cosine", *r.TopScore)
	}
	if !math.IsNaN(value(r.TopScore)) {
		t.Error("the sweep would read a number where the retriever had none")
	}
}

func ids(res ArmResult) []string {
	out := make([]string, 0, len(res.Cases))
	for _, r := range res.Cases {
		out = append(out, r.CaseID)
	}
	return out
}

func outcomes(res ArmResult) []metric.Outcome {
	out := make([]metric.Outcome, 0, len(res.Cases))
	for _, r := range res.Cases {
		out = append(out, r.Outcomes())
	}
	return out
}

// The fakes ledger: every fake in this phase with the test that sets its
// error. P3's sweep found the shape "an error branch behind a fake nobody
// wired", and what the runner does with a failure has to be a value, not "it
// does not crash".
func TestEachRetrievalFailureStopsTheArmAndNamesIt(t *testing.T) {
	cs, spans := ladder()
	boom := errors.New("boom")
	a := arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs, err: boom}, spans)
	_, err := RunArm(context.Background(), a, []golden.Case{caseAt("a", 1, 9)}, cfg())
	if !errors.Is(err, boom) {
		t.Errorf("a failing store gave %v, want the underlying error", err)
	}

	// A repository with nothing to rank is store.ErrNotFound, and the gateway
	// answers that as a refusal. The eval reads it the same way: an outcome,
	// not a failure, or a corpus that is merely empty would fail the run.
	empty := arm(t, "ast", rag.ModeHybrid, &searcher{err: store.ErrNotFound}, spans)
	res, err := RunArm(context.Background(), empty, []golden.Case{caseAt("a", 1, 9)}, cfg())
	if err != nil {
		t.Fatalf("an empty corpus gave %v, want a recorded refusal", err)
	}
	if res.Cases[0].Reason != rag.ReasonNoSpans {
		t.Errorf("an empty corpus decided %q, want no_spans", res.Cases[0].Reason)
	}
}

func TestAFailingEmbedderStopsTheArm(t *testing.T) {
	cs, spans := ladder()
	a := arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs}, spans)
	boom := errors.New("model unreachable")
	a.Retriever.Emb = failingEmbedder{boom}
	_, err := RunArm(context.Background(), a, []golden.Case{caseAt("a", 1, 9)}, cfg())
	if !errors.Is(err, boom) {
		t.Errorf("a failing embedder gave %v, want the underlying error", err)
	}
}

type failingEmbedder struct{ err error }

func (f failingEmbedder) Model() string { return FakeModel }
func (f failingEmbedder) Dim() int      { return store.EmbeddingDim }
func (f failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, f.err
}

func TestAFailedArtefactWriteIsReportedAndNotSwallowed(t *testing.T) {
	// A directory that cannot be created: the artefact is the whole output of
	// the phase, and a write that failed quietly is a run with no evidence.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(sampleRun(), blocked); err == nil {
		t.Error("Write into a path that is a file returned no error")
	}
}

// The lenient and strict ranks are different quantities, and every fixture
// with one gold span per case makes them agree by accident. Measured: with
// gold sets of size one everywhere, computing the lenient MRR from the strict
// rank left every test green.
func TestTheLenientAndStrictRanksAreDifferentQuantities(t *testing.T) {
	cs, spans := ladder()
	// Lines 5-29 clip s1 (5 lines of overlap) and cover s2 and s3 (9 each), so
	// the lenient gold set is {s1,s2,s3} at ranks 1,2,3 and the strict one is
	// s2 — the largest overlap, ties broken by the lower start line.
	res := runOne(t, arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs}, spans),
		[]golden.Case{caseAt("c", 5, 29)}, cfg())
	r := res.Cases[0]
	if len(r.GoldLenient) != 3 {
		t.Fatalf("the lenient gold set is %v, want three spans", r.GoldLenient)
	}
	if r.GoldStrict != "s2" {
		t.Fatalf("the strict gold span is %q, want s2", r.GoldStrict)
	}
	if r.GoldRank != 1 || r.GoldRankStrict != 2 {
		t.Errorf("ranks are lenient %d and strict %d, want 1 and 2", r.GoldRank, r.GoldRankStrict)
	}
	if res.Summary.MRR != 1 || res.Summary.MRRStrict != 0.5 {
		t.Errorf("MRR is %v lenient and %v strict, want 1 and 0.5", res.Summary.MRR, res.Summary.MRRStrict)
	}
	want := []HitRate{{K: 1, Lenient: 1, Strict: 0}, {K: 5, Lenient: 1, Strict: 1}, {K: 10, Lenient: 1, Strict: 1}}
	if !reflect.DeepEqual(res.Summary.HitAt, want) {
		t.Errorf("hit rates are %+v, want %+v", res.Summary.HitAt, want)
	}
	if res.Summary.MeanGoldLenient != 3 {
		t.Errorf("the mean lenient gold-set size is %v, want 3", res.Summary.MeanGoldLenient)
	}
}

// A short result records a short length. Without this the recorded length is a
// side effect nothing reads back, and a truncated list is indistinguishable
// from a corpus that had nothing more to give.
func TestARecordsRankedLengthIsTheListAndNotTheLimit(t *testing.T) {
	cs, spans := ladder()
	res := runOne(t, arm(t, "ast", rag.ModeHybrid, &searcher{vector: cs[:3]}, spans),
		[]golden.Case{caseAt("a", 1, 9)}, cfg())
	r := res.Cases[0]
	if r.Limit != 10 {
		t.Fatalf("the limit is %d; this fixture needs a limit the corpus cannot fill", r.Limit)
	}
	if r.RankedLen != 3 || len(r.Ranked) != 3 {
		t.Errorf("ranked length is %d over a list of %d, want 3 and 3", r.RankedLen, len(r.Ranked))
	}
}

// The sweep's floor list has to be able to land on the value the criterion
// selects. A step that skipped most of the cosine range would make the
// calibration unable to recommend a number the code could then ship.
func TestTheDefaultFloorListCoversTheCosineRangeAtAHundredth(t *testing.T) {
	got, err := parseFloors("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 201 {
		t.Errorf("the default sweep has %d floors, want 201: -1 to 1 at a hundredth", len(got))
	}
	if got[0] != -1 || got[len(got)-1] != 1 {
		t.Errorf("the sweep runs from %v to %v, want -1 to 1", got[0], got[len(got)-1])
	}
	have := map[float64]bool{}
	for _, f := range got {
		have[f] = true
	}
	// Every hundredth in the range a real cosine distribution falls in.
	for _, want := range []float64{-1, 0, 0.5, 0.61, 0.66, 0.7, 0.74, 1} {
		if !have[want] {
			t.Errorf("the sweep never lands on %v", want)
		}
	}
	// And an explicit list is taken verbatim.
	explicit, err := parseFloors("0.1, 0.2")
	if err != nil || !reflect.DeepEqual(explicit, []float64{0.1, 0.2}) {
		t.Errorf("parseFloors(\"0.1, 0.2\") gave %v, %v", explicit, err)
	}
}
