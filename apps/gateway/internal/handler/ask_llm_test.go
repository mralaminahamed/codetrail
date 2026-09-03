package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/agent"
	"github.com/mralaminahamed/codetrail/packages/shared/llm"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// ---- fixtures -------------------------------------------------------------

// fakeCorpus is the store surface the four tools read through. Every tool call
// resolves against the fixture's own spans, so a loop that answers cites
// something a reader could check.
type fakeCorpus struct {
	res    rag.Result
	getErr error
}

func (c *fakeCorpus) Search(_ context.Context, _, _ string, _ int) (rag.Result, error) {
	return c.res, nil
}

func (c *fakeCorpus) GetSpan(_ context.Context, _, spanID string) (models.Span, error) {
	if c.getErr != nil {
		return models.Span{}, c.getErr
	}
	for _, s := range fixtureSpans {
		if s.ID == spanID {
			return s, nil
		}
	}
	return models.Span{}, store.ErrNotFound
}

func (c *fakeCorpus) Definitions(context.Context, string, string, string, bool, int) ([]models.Symbol, error) {
	return nil, nil
}

func (c *fakeCorpus) CallersOf(context.Context, string, string, int, int) ([]store.Caller, error) {
	return nil, nil
}

func (c *fakeCorpus) ApproximateCallersOf(context.Context, string, string, int) ([]store.Approximate, error) {
	return nil, nil
}

// llmHandler is a configured deployment: a scripted model, the fixture corpus,
// and the spend controls at their test values.
func llmHandler(t *testing.T, st *fakeStore, rt Retriever, turns ...llm.Turn) (*Handler, *llm.Fake) {
	t.Helper()
	f := llm.NewFake(turns...)
	h := hermeticHandler(st, rt)
	h.LLM = NewLoop(f, &fakeCorpus{res: result(rag.ModeHybrid, 0.83, true)},
		agent.DefaultBounds(), agent.DefaultToolLimits(), false, 4, 1_000_000, func() time.Time { return fixtureNow })
	h.AnswerDefault = answererLLM
	return h, f
}

func readTurn(id, span string) llm.Turn {
	return llm.Turn{Calls: []llm.ToolCall{{ID: id, Name: "read_span",
		Args: json.RawMessage(`{"span_id":"` + span + `"}`)}}}
}

// answeringTurns is the shortest scripted program that produces a CITED answer:
// open a span, then cite it. An uncited answer is discarded, so a program that
// only answers would exercise the uncited branch by accident.
func answeringTurns(text string) []llm.Turn {
	return []llm.Turn{readTurn("r1", "span-c"), {Text: text}}
}

func ask(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(mount(h), http.MethodPost, "/api/repos/repo-1/ask", body)
}

// ---- the two the brief names ---------------------------------------------

// Driven over EVERY outcome the route has. The field must be present and
// non-empty on all of them, refusals included: P3 put it on the answer shape
// and not on the refusal, which left a refusal unable to say which answerer
// refused.
func TestEveryAskResponseNamesWhatAnsweredIt(t *testing.T) {
	cases := map[string]func(t *testing.T) *httptest.ResponseRecorder{
		"answered extractive": func(t *testing.T) *httptest.ResponseRecorder {
			h := hermeticHandler(newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)})
			return ask(t, h, `{"q":"sampler"}`)
		},
		"answered llm": func(t *testing.T) *httptest.ResponseRecorder {
			h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
				answeringTurns("the sampler drops events [1]")...)
			return ask(t, h, `{"q":"sampler"}`)
		},
		"refused no_spans": func(t *testing.T) *httptest.ResponseRecorder {
			h := hermeticHandler(newStore(), &fakeRetriever{res: rag.Result{Mode: rag.ModeHybrid, VectorRan: true}})
			return ask(t, h, `{"q":"sampler"}`)
		},
		"refused below_floor": func(t *testing.T) *httptest.ResponseRecorder {
			h := hermeticHandler(newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.2, true)})
			h.Floor = rag.Floor{Value: 0.5}
			return ask(t, h, `{"q":"sampler"}`)
		},
		"refused unscored": func(t *testing.T) *httptest.ResponseRecorder {
			h := hermeticHandler(newStore(), &fakeRetriever{res: unscoredResult()})
			return ask(t, h, `{"q":"sampler"}`)
		},
	}
	for _, stop := range degradations() {
		name, turn := stop.name, stop.turn
		cases["degraded "+name] = func(t *testing.T) *httptest.ResponseRecorder {
			h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}, turn...)
			return ask(t, h, `{"q":"sampler"}`)
		}
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			rec := run(t)
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
			}
			out := body(t, rec)
			got, ok := out["answered_by"].(string)
			if !ok {
				t.Fatalf("response has no answered_by key: %s", rec.Body)
			}
			if got == "" {
				t.Fatalf("answered_by is empty: %s", rec.Body)
			}
			if !slices.Contains(metrics.Answerers, got) {
				t.Errorf("answered_by %q is not one of %v", got, metrics.Answerers)
			}
		})
	}
}

// Asserted from the OTHER SIDE as well as from the field: a fake that is never
// called. A mutant that hard-wires "llm" is caught by the field, and one that
// calls the model anyway is caught by the recorder.
func TestAnExtractiveAnswerNeverClaimsToBeTheModel(t *testing.T) {
	// The model fails on turn 1, so extractive answers.
	h, f := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		llm.Turn{Err: &llm.Failure{Kind: llm.KindRateLimited, Status: 429}})
	rec := ask(t, h, `{"q":"sampler"}`)
	out := body(t, rec)
	if out["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v with a failed model: the response claims the model wrote an answer the extractive path wrote", out["answered_by"])
	}
	deg, _ := out["degraded"].(map[string]any)
	if deg == nil || deg["reason"] != "rate_limited" || deg["from"] != "llm" {
		t.Errorf("degraded %v, want {from:llm reason:rate_limited}", out["degraded"])
	}

	// And the other direction: a caller who asked for extractive on a configured
	// deployment. The recorder is the assertion — the response would read
	// "extractive" under a mutant that called the model anyway.
	h2, f2 := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns("never reached [1]")...)
	rec2 := ask(t, h2, `{"q":"sampler","answerer":"extractive"}`)
	out2 := body(t, rec2)
	if out2["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v", out2["answered_by"])
	}
	if n := len(f2.Requests()); n != 0 {
		t.Errorf("the model was called %d times for an explicit answerer=extractive, want 0", n)
	}
	if _, ok := out2["degraded"]; ok {
		t.Errorf("a caller who asked for extractive was told the answer was degraded: %s", rec2.Body)
	}
	if _, ok := out2["llm"]; ok {
		t.Errorf("the llm block is present although the loop did not run: %s", rec2.Body)
	}
	_ = f
}

// ---- the nine degradations, one branch each -------------------------------

type degradation struct {
	name string
	turn []llm.Turn
}

// Each is a different branch. Three of them — malformed_tool_call, tool_error
// and repeated_tool_call — never reach the provider at all, which is why one
// shared "it degrades" test would not be evidence.
func degradations() []degradation {
	// DISTINGUISHABLE malformed calls: four byte-identical ones would trip the
	// repeat guard first and this fixture would measure that instead. Each
	// carries the unknown `repo` key an injection would use.
	bad := func(i int) llm.Turn {
		return llm.Turn{Calls: []llm.ToolCall{{ID: fmt.Sprint("b", i), Name: "read_span",
			Args: json.RawMessage(fmt.Sprintf(`{"span_id":"x%d","repo":"repo-2"}`, i))}}}
	}
	same := readTurn("s", "span-c")
	unknown := func(i int) llm.Turn {
		return llm.Turn{Calls: []llm.ToolCall{{ID: fmt.Sprint("u", i), Name: "delete_repo",
			Args: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))}}}
	}
	step := make([]llm.Turn, 0, 8)
	for i := range 8 {
		step = append(step, llm.Turn{Calls: []llm.ToolCall{{ID: fmt.Sprint(i), Name: "search_code",
			Args: json.RawMessage(fmt.Sprintf(`{"q":"term%d"}`, i))}}})
	}
	return []degradation{
		{"rate_limited", []llm.Turn{{Err: &llm.Failure{Kind: llm.KindRateLimited, Status: 429}}}},
		{"unauthorized", []llm.Turn{{Err: &llm.Failure{Kind: llm.KindUnauthorized, Status: 401}}}},
		{"provider_unavailable", []llm.Turn{{Err: &llm.Failure{Kind: llm.KindUnavailable, Status: 503}}}},
		{"deadline", []llm.Turn{{Err: &llm.Failure{Kind: llm.KindDeadline}}}},
		{"malformed_response", []llm.Turn{{Err: &llm.Failure{Kind: llm.KindMalformed}}}},
		{"malformed_tool_call", []llm.Turn{bad(1), bad(2), bad(3), bad(4)}},
		{"tool_error", []llm.Turn{readTurn("e", "span-c")}}, // the corpus errors; see below
		{"repeated_tool_call", []llm.Turn{same, same, same, same}},
		{"step_limit", step},
		{"uncited", []llm.Turn{{Text: "I read the code and it looks fine."}}},
		{"unknown_tool", []llm.Turn{unknown(1), unknown(2), unknown(3), unknown(4)}},
	}
}

func TestEachDegradationDegradesToExtractiveAndSaysWhich(t *testing.T) {
	want := map[string]string{
		"rate_limited": "rate_limited", "unauthorized": "unauthorized",
		"provider_unavailable": "provider_unavailable", "deadline": "deadline",
		"malformed_response": "malformed_response", "malformed_tool_call": "malformed_tool_call",
		"tool_error": "tool_error", "repeated_tool_call": "repeated_tool_call",
		"step_limit": "step_limit", "uncited": "uncited",
		"unknown_tool": "malformed_tool_call",
	}
	for _, d := range degradations() {
		t.Run(d.name, func(t *testing.T) {
			h, f := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}, d.turn...)
			if d.name == "tool_error" {
				h.LLM.(*Loop).Corpus.(*fakeCorpus).getErr = errors.New("pool exhausted")
			}
			rec := ask(t, h, `{"q":"sampler"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
			}
			out := body(t, rec)
			if out["answered_by"] != answererExtractive {
				t.Errorf("answered_by %v, want extractive", out["answered_by"])
			}
			if out["refused"] != false {
				t.Errorf("refused %v: a degradation is not a refusal", out["refused"])
			}
			deg, _ := out["degraded"].(map[string]any)
			if deg == nil {
				t.Fatalf("no degraded block: %s", rec.Body)
			}
			if deg["reason"] != want[d.name] {
				t.Errorf("degraded.reason %v, want %q", deg["reason"], want[d.name])
			}
			// The llm block is present even though the loop did not write the
			// answer, so a reader sees the work done before the fallback.
			tr, _ := out["llm"].(map[string]any)
			if tr == nil {
				t.Fatalf("no llm block on a degraded answer: %s", rec.Body)
			}
			if tr["stop"] != want[d.name] {
				t.Errorf("llm.stop %v, want %q", tr["stop"], want[d.name])
			}
			// And a cited extractive answer really was served.
			if s, _ := out["answer"].(string); s == "" {
				t.Errorf("the degraded response carries no extractive answer: %s", rec.Body)
			}
			if cs, _ := out["citations"].([]any); len(cs) == 0 {
				t.Errorf("the degraded response carries no citations: %s", rec.Body)
			}
			_ = f
		})
	}
}

// The set is closed: a tenth reason added later without a payload field fails
// here. Derived from agent.Stops rather than from a literal, so the two
// vocabularies cannot drift.
func TestEveryStopReasonDegradesToExtractiveAndSaysWhich(t *testing.T) {
	covered := map[string]bool{}
	for _, d := range degradations() {
		h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}, d.turn...)
		if d.name == "tool_error" {
			h.LLM.(*Loop).Corpus.(*fakeCorpus).getErr = errors.New("pool exhausted")
		}
		out := body(t, ask(t, h, `{"q":"sampler"}`))
		if deg, ok := out["degraded"].(map[string]any); ok {
			covered[deg["reason"].(string)] = true
		}
	}
	// busy and budget_exhausted have their own tests below; final is not a
	// degradation; tool_call_limit and token_budget are ceilings the loop's own
	// suite drives at N-1/N/N+1.
	elsewhere := map[agent.Stop]bool{
		agent.StopFinal: true, agent.StopBusy: true, agent.StopBudgetExhausted: true,
		agent.StopToolCallLimit: true, agent.StopTokenBudget: true,
	}
	for _, s := range agent.Stops {
		if elsewhere[s] {
			continue
		}
		if !covered[string(s)] {
			t.Errorf("stop reason %q has no degradation test: a tenth reason cannot be added without one", s)
		}
	}
	// And the metric vocabulary carries every one of them, so a new reason
	// cannot ship without a series that initialises to zero.
	for _, s := range agent.Stops {
		if !slices.Contains(metrics.LLMStopReasons, string(s)) {
			t.Errorf("agent.Stops carries %q and metrics.LLMStopReasons does not", s)
		}
	}
	for _, r := range metrics.LLMStopReasons {
		if !slices.Contains(agent.Stops, agent.Stop(r)) {
			t.Errorf("metrics.LLMStopReasons carries %q and agent.Stops does not", r)
		}
	}
}

// ---- not configured, asked for extractive, tried and failed ---------------

func TestAnUnconfiguredDeploymentIsNotADegradedOne(t *testing.T) {
	h := hermeticHandler(newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)})
	rec := ask(t, h, `{"q":"sampler"}`)
	out := body(t, rec)
	if out["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v", out["answered_by"])
	}
	if v, ok := out["degraded"]; ok {
		t.Errorf("degraded %v on a deployment with no provider: every offline deploy would read as broken", v)
	}
	if v, ok := out["llm"]; ok {
		t.Errorf("llm %v on a deployment with no provider", v)
	}
}

func TestAskingForLlmOnAnUnconfiguredDeploymentIsFourHundredNamingTheRule(t *testing.T) {
	h := hermeticHandler(newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)})
	rt := h.Rag.(*fakeRetriever)
	rec := ask(t, h, `{"q":"sampler","answerer":"llm"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
	}
	out := body(t, rec)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "no model provider configured") {
		t.Errorf("the 400 does not name the rule: %s", rec.Body)
	}
	// Not a silent extractive answer: the caller asked for something this
	// deployment does not have, which is a different fact from something it
	// tried and could not do.
	if len(rt.calls) != 0 {
		t.Errorf("a refused request retrieved anyway: %v", rt.calls)
	}
}

func TestAnUnknownAnswererIsFourHundred(t *testing.T) {
	h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns("never [1]")...)
	for _, v := range []string{`"gpt"`, `""`, `"LLM"`, `null`} {
		rec := ask(t, h, `{"q":"sampler","answerer":`+v+`}`)
		if v == `null` {
			// An explicit null is absence, as it is for mode and limit.
			if rec.Code != http.StatusOK {
				t.Errorf("answerer:null answered %d: %s", rec.Code, rec.Body)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("answerer:%s answered %d: %s", v, rec.Code, rec.Body)
		}
	}
}

func TestAskingForExtractiveOnAConfiguredDeploymentCallsNoModel(t *testing.T) {
	h, f := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns("never reached [1]")...)
	out := body(t, ask(t, h, `{"q":"sampler","answerer":"extractive"}`))
	if out["answered_by"] != answererExtractive || len(f.Requests()) != 0 {
		t.Errorf("answered_by %v with %d model calls, want extractive with 0", out["answered_by"], len(f.Requests()))
	}

	// The inverse, and the recorder is what makes it evidence: under a mutant
	// that binds answerer and ignores it, the response would read "extractive"
	// and LOOK consistent.
	h2, f2 := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns("the sampler drops events [1]")...)
	h2.AnswerDefault = answererExtractive
	out2 := body(t, ask(t, h2, `{"q":"sampler","answerer":"llm"}`))
	if n := len(f2.Requests()); n == 0 {
		t.Errorf("the model was called %d times for an explicit answerer=llm, want it called", n)
	}
	if out2["answered_by"] != answererLLM {
		t.Errorf("answered_by %v, want llm", out2["answered_by"])
	}
}

func TestTheDefaultAnswererIsExtractiveEvenWithAProviderConfigured(t *testing.T) {
	h, f := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns("never reached [1]")...)
	h.AnswerDefault = answererExtractive
	out := body(t, ask(t, h, `{"q":"sampler"}`))
	if out["answered_by"] != answererExtractive || len(f.Requests()) != 0 {
		t.Errorf("a default-answerer request cost %d model calls and read %v", len(f.Requests()), out["answered_by"])
	}
}

// ---- spend ----------------------------------------------------------------

// blockingModel holds the first call until release is closed, so a second
// request meets a full semaphore.
type blockingModel struct {
	inner   *llm.Fake
	release chan struct{}
	once    sync.Once
}

func (b *blockingModel) Name() string { return b.inner.Name() }

func (b *blockingModel) Complete(ctx context.Context, r llm.Request) (llm.Response, error) {
	b.once.Do(func() { <-b.release })
	return b.inner.Complete(ctx, r)
}

func TestTheConcurrencyCapDegradesRatherThanQueues(t *testing.T) {
	release := make(chan struct{})
	// Closed in a Cleanup so the held request always finishes, whichever way
	// the assertion went.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	inner := llm.NewFake(append(answeringTurns("first [1]"), answeringTurns("second [1]")...)...)
	h := hermeticHandler(newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)})
	h.LLM = NewLoop(&blockingModel{inner: inner, release: release},
		&fakeCorpus{res: result(rag.ModeHybrid, 0.83, true)},
		agent.DefaultBounds(), agent.DefaultToolLimits(), false, 1, 1_000_000,
		func() time.Time { return fixtureNow })
	h.AnswerDefault = answererLLM
	e := mount(h)

	held := make(chan struct{})
	go func() {
		defer close(held)
		do(e, http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`)
	}()
	// The first request has to be inside the semaphore before the second
	// arrives, or this measures nothing.
	waitFor(t, func() bool { return inner != nil && len(inner.Requests()) == 0 })

	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() { done <- do(e, http.MethodPost, "/api/repos/repo-1/ask", `{"q":"sampler"}`) }()

	// Without this select the QUEUEING mutant hangs the suite for ten minutes
	// and the "kill" is a panic rather than an assertion.
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatalf("the second ask did not return within 2s; want answered_by extractive with degraded.reason busy in under 500ms")
	}
	elapsed := time.Since(start)
	out := body(t, rec)
	if out["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v, want extractive", out["answered_by"])
	}
	deg, _ := out["degraded"].(map[string]any)
	if deg == nil || deg["reason"] != string(agent.StopBusy) {
		t.Errorf("degraded %v, want reason busy", out["degraded"])
	}
	// Elapsed as well as the reason: without it "degraded immediately" and
	// "queued and got lucky" are the same result.
	if elapsed > 500*time.Millisecond {
		t.Errorf("the second ask returned after %v, want under 500ms", elapsed)
	}
	close(release)
	<-held
}

func TestAnExhaustedTokenBudgetDegradesAndSaysSo(t *testing.T) {
	h := hermeticHandler(newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)})
	// A budget under one worst-case request, so the first ask is refused before
	// any model call rather than after one.
	f := llm.NewFake(answeringTurns("never [1]")...)
	h.LLM = NewLoop(f, &fakeCorpus{res: result(rag.ModeHybrid, 0.83, true)},
		agent.DefaultBounds(), agent.DefaultToolLimits(), false, 2, 100,
		func() time.Time { return fixtureNow })
	h.AnswerDefault = answererLLM

	out := body(t, ask(t, h, `{"q":"sampler"}`))
	if out["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v", out["answered_by"])
	}
	deg, _ := out["degraded"].(map[string]any)
	if deg == nil || deg["reason"] != string(agent.StopBudgetExhausted) {
		t.Errorf("degraded %v, want reason budget_exhausted", out["degraded"])
	}
	// Before the call, not after: a bound checked after the spend is a report.
	if n := len(f.Requests()); n != 0 {
		t.Errorf("the model was called %d times under an exhausted budget, want 0", n)
	}
}

// The budget spends REPORTED usage where the provider reports it, and the
// fixture's reported usage is deliberately far from len(text)/4 — equal numbers
// could not separate the two.
func TestTheBudgetIsSpentFromReportedUsageWhenTheProviderReportsIt(t *testing.T) {
	now := fixtureNow
	b := newBudget(100_000, func() time.Time { return now })
	b.spend(9000)
	if got := b.remaining(); got != 91_000 {
		t.Errorf("budget_remaining %d, want 91000", got)
	}

	// And through the loop: a turn reporting 9,000 input tokens against an
	// estimate of a few hundred.
	f := llm.NewFake(
		llm.Turn{Calls: readTurn("r", "span-c").Calls, Usage: llm.Usage{InputTokens: 9000, OutputTokens: 0}},
		llm.Turn{Text: "answer [1]", Usage: llm.Usage{InputTokens: 0, OutputTokens: 0}},
	)
	l := NewLoop(f, &fakeCorpus{res: result(rag.ModeHybrid, 0.83, true)},
		agent.DefaultBounds(), agent.DefaultToolLimits(), false, 2, 100_000,
		func() time.Time { return now })
	a := l.Ask(context.Background(), "repo-1", "sampler", rag.Result{})
	if a.Trace.Stop != agent.StopFinal {
		t.Fatalf("stop %q: %+v", a.Trace.Stop, a.Trace)
	}
	if a.Trace.Usage.InputTokens < 9000 {
		t.Errorf("usage %+v, want the provider's 9000 input tokens", a.Trace.Usage)
	}
	if got, want := l.budget.remaining(), 100_000-(a.Trace.Usage.InputTokens+a.Trace.Usage.OutputTokens); got != want {
		t.Errorf("budget_remaining %d, want %d", got, want)
	}
	if l.budget.remaining() > 99_000 {
		t.Errorf("budget_remaining %d: the reported 9,000 tokens were not spent", l.budget.remaining())
	}
}

func TestTheBudgetWindowRolls(t *testing.T) {
	now := fixtureNow
	b := newBudget(1000, func() time.Time { return now })
	b.spend(900)
	if b.allow(200) {
		t.Errorf("a request over the budget was allowed")
	}
	now = now.Add(61 * time.Minute)
	if !b.allow(200) {
		t.Errorf("the budget did not roll after an hour")
	}
	if got := b.remaining(); got != 1000 {
		t.Errorf("budget_remaining %d after the window rolled, want 1000", got)
	}
}

// ---- the floor and the refusal path ---------------------------------------

func TestNoSpansRefusesBeforeAnyModelCall(t *testing.T) {
	h, f := llmHandler(t, newStore(), &fakeRetriever{res: rag.Result{Mode: rag.ModeHybrid, VectorRan: true}},
		answeringTurns("never [1]")...)
	out := body(t, ask(t, h, `{"q":"sampler"}`))
	if out["refused"] != true || out["reason"] != string(rag.ReasonNoSpans) {
		t.Errorf("response %s, want a no_spans refusal", rec2s(out))
	}
	// The recorder, because the response is a refusal either way and the
	// payload cannot tell.
	if n := len(f.Requests()); n != 0 {
		t.Errorf("the model was called %d time(s) on a no_spans refusal, want 0", n)
	}
	if out["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v", out["answered_by"])
	}
	if _, ok := out["degraded"]; ok {
		t.Errorf("a short-circuit is not a degradation: %v", out["degraded"])
	}
}

// An explicit floor, because at the shipped -1 this branch is unreachable and a
// fixture at the default cannot fire.
func TestABelowFloorRefusalDoesNotRunTheLoopWhenTheKnobIsOff(t *testing.T) {
	h, f := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.2, true)},
		answeringTurns("never [1]")...)
	h.Floor = rag.Floor{Value: 0.5}
	out := body(t, ask(t, h, `{"q":"sampler"}`))
	if out["refused"] != true || out["reason"] != string(rag.ReasonBelowFloor) {
		t.Errorf("response %s, want a below_floor refusal", rec2s(out))
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("the model was called %d time(s) under a below_floor refusal with LLM_BELOW_FLOOR=false, want 0", n)
	}
}

// Unreachable at the shipped knob, which is exactly why it needs a test: with
// LLM_BELOW_FLOOR=true a degradation must NOT fall through into an answer to a
// question the floor refused.
func TestABelowFloorRefusalStillRefusesWhenTheLoopDegrades(t *testing.T) {
	h, f := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.2, true)},
		llm.Turn{Err: &llm.Failure{Kind: llm.KindRateLimited, Status: 429}})
	h.Floor = rag.Floor{Value: 0.5}
	h.LLM.(*Loop).BelowFloor = true
	out := body(t, ask(t, h, `{"q":"sampler"}`))
	if n := len(f.Requests()); n != 1 {
		t.Fatalf("the model was called %d times with LLM_BELOW_FLOOR=true, want 1", n)
	}
	if out["refused"] != true {
		t.Errorf("the degraded loop answered a question the floor refused: %s", rec2s(out))
	}
	if out["reason"] != string(rag.ReasonBelowFloor) {
		t.Errorf("reason %v, want below_floor", out["reason"])
	}
	// The refusal still says which answerer refused, and that the loop ran.
	if out["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v", out["answered_by"])
	}
	deg, _ := out["degraded"].(map[string]any)
	if deg == nil || deg["reason"] != "rate_limited" {
		t.Errorf("degraded %v on a refusal after a failed loop", out["degraded"])
	}
	if _, ok := out["llm"]; !ok {
		t.Errorf("no llm block on a refusal after the loop ran: %s", rec2s(out))
	}
}

func TestARefusalNamesWhichAnswererRefused(t *testing.T) {
	h := hermeticHandler(newStore(), &fakeRetriever{res: rag.Result{Mode: rag.ModeHybrid, VectorRan: true}})
	out := body(t, ask(t, h, `{"q":"sampler"}`))
	if _, ok := out["answered_by"]; !ok {
		t.Fatalf("refusal response has no answered_by key: %s", rec2s(out))
	}
	if out["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v", out["answered_by"])
	}
}

// ---- hygiene --------------------------------------------------------------

func TestTheQuestionAndThePromptAreNeverLogged(t *testing.T) {
	const question = "CANARY-QUESTION"
	const answer = "CANARY-ANSWER"
	var logged bytes.Buffer
	h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		readTurn("r", "span-c"), llm.Turn{Text: answer + " with no marker"})
	h.Log = zerolog.New(&logged).Level(zerolog.DebugLevel)
	rec := ask(t, h, `{"q":"`+question+` sampler"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	// The fixture is proved able to fire: the loop DID degrade, so the log line
	// under test was written.
	if !strings.Contains(logged.String(), "degraded") {
		t.Fatalf("the degradation was never logged, so this test proves nothing: %s", logged.String())
	}
	for _, canary := range []string{question, answer} {
		if strings.Contains(logged.String(), canary) {
			t.Errorf("the log line contains %q: %s", canary, logged.String())
		}
	}
	// What it does carry, so the absence above is not simply an empty line.
	for _, want := range []string{"stop", "steps", "tool_calls", "repo_id"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the degradation log line does not carry %q: %s", want, logged.String())
		}
	}

	// AND THE ANSWERED PATH, because on the degraded one the assertion above is
	// half vacuous: Answer.Text is empty for every stop but final, so a mutant
	// logging it on the degradation branch leaks nothing. Measured — the
	// question canary fired and the answer canary did not. The path where the
	// model's prose actually exists is the one that answers.
	var answered bytes.Buffer
	h2, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns(answer+" [1]")...)
	h2.Log = zerolog.New(&answered).Level(zerolog.DebugLevel)
	rec2 := ask(t, h2, `{"q":"`+question+` sampler"}`)
	out2 := body(t, rec2)
	if out2["answered_by"] != answererLLM {
		t.Fatalf("the answered fixture did not answer from the model, so this half proves nothing: %s", rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), answer) {
		t.Fatalf("the model's text never reached the response, so the canary is not detectable at all")
	}
	for _, canary := range []string{question, answer} {
		if strings.Contains(answered.String(), canary) {
			t.Errorf("an answered request logged %q: %s", canary, answered.String())
		}
	}
}

func TestTheLlmBlockIsAbsentWhenTheLoopDidNotRun(t *testing.T) {
	for name, h := range map[string]*Handler{
		"no provider": hermeticHandler(newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)}),
	} {
		t.Run(name, func(t *testing.T) {
			out := body(t, ask(t, h, `{"q":"sampler"}`))
			if v, ok := out["llm"]; ok {
				t.Errorf("llm %v although the loop did not run", v)
			}
		})
	}
	// And on a configured deployment where the caller asked for extractive.
	h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns("never [1]")...)
	out := body(t, ask(t, h, `{"q":"sampler","answerer":"extractive"}`))
	if v, ok := out["llm"]; ok {
		t.Errorf("llm %v although the caller asked for extractive", v)
	}
}

// codetrail_answer_total keeps its three outcomes. A degradation is ANSWERED,
// not error: the caller got an answer, and filing it as an error inflates
// exactly the rate spec:262-263 wants readable.
func TestTheAnswerCounterStillHasThreeOutcomesAndADegradationIsAnswered(t *testing.T) {
	before := llmCounters(t)
	h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		llm.Turn{Err: &llm.Failure{Kind: llm.KindRateLimited, Status: 429}})
	if rec := ask(t, h, `{"q":"sampler"}`); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	// The WHOLE delta vector, so a mutant that increments two series fails even
	// when the one expected is right.
	moved := llmMovedSince(t, before)
	want := map[string]float64{
		`codetrail_answer_total{outcome="answered"}`:       1,
		`codetrail_answer_by_total{answerer="extractive"}`: 1,
		`codetrail_llm_stop_total{reason="rate_limited"}`:  1,
	}
	for series, delta := range want {
		if moved[series] != delta {
			t.Errorf("%s moved %v, want %v", series, moved[series], delta)
		}
		delete(moved, series)
	}
	for series, delta := range moved {
		t.Errorf("%s moved %v, want 0", series, delta)
	}
}

func TestAnAnsweredLoopCountsAsLlmOnTheAnswererCounter(t *testing.T) {
	before := llmCounters(t)
	h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns("the sampler drops events [1]")...)
	if rec := ask(t, h, `{"q":"sampler"}`); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	moved := llmMovedSince(t, before)
	want := map[string]float64{
		`codetrail_answer_total{outcome="answered"}`: 1,
		`codetrail_answer_by_total{answerer="llm"}`:  1,
		`codetrail_llm_stop_total{reason="final"}`:   1,
	}
	for series, delta := range want {
		if moved[series] != delta {
			t.Errorf("%s moved %v, want %v", series, moved[series], delta)
		}
		delete(moved, series)
	}
	for series, delta := range moved {
		t.Errorf("%s moved %v, want 0", series, delta)
	}
}

// ---- the answered payload -------------------------------------------------

func TestAnAnsweredLoopCitesTheSpansItOpened(t *testing.T) {
	h, _ := llmHandler(t, newStore(), &fakeRetriever{res: result(rag.ModeHybrid, 0.83, true)},
		answeringTurns("the sampler drops events [1]")...)
	rec := ask(t, h, `{"q":"sampler"}`)
	out := body(t, rec)
	if out["answered_by"] != answererLLM {
		t.Fatalf("answered_by %v: %s", out["answered_by"], rec.Body)
	}
	cs, _ := out["citations"].([]any)
	if len(cs) != 1 {
		t.Fatalf("citations %v, want one", out["citations"])
	}
	c, _ := cs[0].(map[string]any)
	if c["span_id"] != "span-c" {
		t.Errorf("citation names span %v, want span-c: the loop opened that one", c["span_id"])
	}
	if c["marker"] != float64(1) {
		t.Errorf("marker %v, want 1", c["marker"])
	}
	// The citation carries the digest a reader can check against git show.
	cit, _ := c["citation"].(map[string]any)
	if cit == nil || cit["digest"] == "" {
		t.Errorf("the citation carries no digest: %s", rec.Body)
	}
	tr, _ := out["llm"].(map[string]any)
	if tr == nil || tr["stop"] != "final" || tr["model"] != "fake-scripted" {
		t.Errorf("llm %v", out["llm"])
	}
	if _, ok := out["degraded"]; ok {
		t.Errorf("degraded is present on an answer the model wrote: %s", rec.Body)
	}
}

// ---- helpers --------------------------------------------------------------

func unscoredResult() rag.Result {
	r := result(rag.ModeHybrid, 0.83, true)
	r.TopScore = math.NaN()
	return r
}

func rec2s(out map[string]any) string {
	b, _ := json.Marshal(out)
	return string(b)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			time.Sleep(20 * time.Millisecond)
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the fixture never reached the state the assertion needs")
}

// llmCounters reads the five new instruments as well as P3's two, so a whole
// delta vector really is whole.
func llmCounters(t *testing.T) map[string]float64 {
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
			!strings.HasPrefix(line, "codetrail_refusal_total{") &&
			!strings.HasPrefix(line, "codetrail_answer_by_total{") &&
			!strings.HasPrefix(line, "codetrail_llm_stop_total{") {
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
		t.Fatal("no answer counters are registered; the assertions would prove nothing")
	}
	return out
}

func llmMovedSince(t *testing.T, before map[string]float64) map[string]float64 {
	t.Helper()
	moved := map[string]float64{}
	for series, now := range llmCounters(t) {
		if d := now - before[series]; d != 0 {
			moved[series] = d
		}
	}
	return moved
}
