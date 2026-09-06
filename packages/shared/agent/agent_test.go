package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/llm"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// ---- fixtures -------------------------------------------------------------

var toolNames = []string{"search_code", "read_span", "definition_of", "callers_of"}

// tools is a ToolSet with a recorder. The recorder is what measures work
// actually done: a guard that swallows a call still leaves the trace looking
// right, and only a count of dispatches can see it.
type tools struct {
	calls []llm.ToolCall
	fn    func(n int, c llm.ToolCall) (Result, error)
}

func (ts *tools) Specs() []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(toolNames))
	for _, n := range toolNames {
		out = append(out, llm.ToolSpec{Name: n, Description: "d", Schema: json.RawMessage(`{}`)})
	}
	return out
}

// A tool call opens a span unless the script says otherwise, so a final answer
// has something to cite: an answer that resolves no citation is discarded, and
// a fixture whose tools open nothing would exercise that branch by accident in
// every ceiling test.
func (ts *tools) Call(_ context.Context, c llm.ToolCall) (Result, error) {
	ts.calls = append(ts.calls, c)
	if ts.fn != nil {
		return ts.fn(len(ts.calls), c)
	}
	return Result{Content: `{"ok":true}`, Read: []models.Span{{ID: fmt.Sprintf("s%d", len(ts.calls))}}}, nil
}

func okTools() *tools { return &tools{} }

// readTools opens a span named by the call's span_id, so Read is drivable from
// the script.
func readTools() *tools {
	return &tools{fn: func(_ int, c llm.ToolCall) (Result, error) {
		var a struct {
			SpanID string `json:"span_id"`
		}
		_ = json.Unmarshal(c.Args, &a)
		if a.SpanID == "" {
			return Result{Content: `{"ok":true}`}, nil
		}
		return Result{Content: `{"span":"` + a.SpanID + `"}`, Read: []models.Span{{ID: a.SpanID, Text: "t-" + a.SpanID}}}, nil
	}}
}

func toolTurn(i int) llm.Turn {
	name := toolNames[i%len(toolNames)]
	return llm.Turn{Calls: []llm.ToolCall{{
		ID: fmt.Sprintf("c%d", i), Name: name,
		Args: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
	}}}
}

// toolTurns is n distinguishable turns: a different tool name each step and
// different arguments, so Trace.Tools is a list a test can compare element by
// element and the repeat guard never fires by accident.
func toolTurns(n int) []llm.Turn {
	out := make([]llm.Turn, 0, n)
	for i := range n {
		out = append(out, toolTurn(i))
	}
	return out
}

func expectedTools(n int) []ToolInvocation {
	out := make([]ToolInvocation, 0, n)
	for i := range n {
		out = append(out, ToolInvocation{Name: toolNames[i%len(toolNames)]})
	}
	return out
}

func names(inv []ToolInvocation) []string {
	out := make([]string, 0, len(inv))
	for _, i := range inv {
		out = append(out, i.Name)
	}
	return out
}

func readTurn(id, span string) llm.Turn {
	return llm.Turn{Calls: []llm.ToolCall{{ID: id, Name: "read_span", Args: json.RawMessage(`{"span_id":"` + span + `"}`)}}}
}

func finalTurn(text string) llm.Turn { return llm.Turn{Text: text} }

func testBounds() Bounds {
	b := DefaultBounds()
	b.Deadline = 10 * time.Second
	return b
}

// assertLoop checks the three observers every ceiling gets: the stop reason,
// where it stopped, and the work the model was actually asked to do.
func assertLoop(t *testing.T, a Answer, f *llm.Fake, stop Stop, steps, requests int) {
	t.Helper()
	if a.Trace.Stop != stop {
		t.Errorf("stop %q at step %d, want %q", a.Trace.Stop, a.Trace.Steps, stop)
	}
	if a.Trace.Steps != steps {
		t.Errorf("trace.steps %d, want %d", a.Trace.Steps, steps)
	}
	if n := len(f.Requests()); n != requests {
		t.Errorf("the model was called %d times, want %d", n, requests)
	}
}

// ---- the loop -------------------------------------------------------------

func TestALoopThatGetsAFinalAnswerStopsWithFinalAndNamesTheToolsItUsed(t *testing.T) {
	// Three DIFFERENT spans, then an answer. A fixture that read one span twice
	// could not tell a byte-identical repeat guard from a name-only one.
	f := llm.NewFake(
		readTurn("c1", "read-a"), readTurn("c2", "read-b"), readTurn("c3", "read-c"),
		finalTurn("it does this [1]"),
	)
	ts := readTools()
	a := Run(context.Background(), f, ts, testBounds(), "what does it do")

	assertLoop(t, a, f, StopFinal, 4, 4)
	if a.Text != "it does this [1]" {
		t.Errorf("text %q", a.Text)
	}
	// The recorder, not the trace: a name-only repeat guard leaves the trace
	// identical and dispatches once.
	if len(ts.calls) != 3 {
		t.Errorf("the tool set was called %d times, want 3", len(ts.calls))
	}
	if want := []string{"read_span", "read_span", "read_span"}; !reflect.DeepEqual(names(a.Trace.Tools), want) {
		t.Errorf("trace.tools %v, want %v", names(a.Trace.Tools), want)
	}
	if a.Trace.ToolCalls != 3 {
		t.Errorf("trace.tool_calls %d, want 3", a.Trace.ToolCalls)
	}
	if a.Trace.Model != "fake-scripted" {
		t.Errorf("trace.model %q", a.Trace.Model)
	}
}

// N-1, N and N+1, against a program strictly longer than any of them: a program
// of exactly MaxSteps turns ends before the ceiling does and passes under a
// mutant that raises it.
func TestTheStepCeilingStopsAtNAndTheTraceSaysWhereItStopped(t *testing.T) {
	for _, maxSteps := range []int{5, 6, 7} {
		t.Run(fmt.Sprint(maxSteps), func(t *testing.T) {
			f := llm.NewFake(toolTurns(8)...)
			b := testBounds()
			b.MaxSteps = maxSteps
			a := Run(context.Background(), f, okTools(), b, "q")

			assertLoop(t, a, f, StopStepLimit, maxSteps, maxSteps)
			if !reflect.DeepEqual(a.Trace.Tools, expectedTools(maxSteps)) {
				t.Errorf("trace.tools %v, want %v", names(a.Trace.Tools), names(expectedTools(maxSteps)))
			}
		})
	}
}

func TestOneStepUnderTheCeilingStillAnswers(t *testing.T) {
	turns := append(toolTurns(5), finalTurn("answer [1]"))
	f := llm.NewFake(turns...)
	b := testBounds()
	b.MaxSteps = 6
	a := Run(context.Background(), f, okTools(), b, "q")
	assertLoop(t, a, f, StopFinal, 6, 6)
	if a.Text != "answer [1]" {
		t.Errorf("text %q, want the answer: the just-under case must succeed or the ceiling test proves nothing", a.Text)
	}
}

func TestTheToolCallCeilingBindsWhenOneStepRequestsMany(t *testing.T) {
	many := llm.Turn{}
	for i := range 5 {
		many.Calls = append(many.Calls, llm.ToolCall{
			ID: fmt.Sprintf("m%d", i), Name: toolNames[i%len(toolNames)],
			Args: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		})
	}
	for _, tc := range []struct {
		max        int
		stop       Stop
		steps      int
		reqs       int
		dispatched int
	}{
		{4, StopToolCallLimit, 1, 1, 4},
		{5, StopFinal, 2, 2, 5},
		{6, StopFinal, 2, 2, 5},
	} {
		t.Run(fmt.Sprint(tc.max), func(t *testing.T) {
			f := llm.NewFake(many, finalTurn("done [1]"))
			ts := okTools()
			b := testBounds()
			b.MaxToolCalls = tc.max
			a := Run(context.Background(), f, ts, b, "q")
			assertLoop(t, a, f, tc.stop, tc.steps, tc.reqs)
			if len(ts.calls) != tc.dispatched {
				t.Errorf("the tool set was called %d times, want %d", len(ts.calls), tc.dispatched)
			}
			if a.Trace.ToolCalls != tc.dispatched {
				t.Errorf("trace.tool_calls %d, want %d", a.Trace.ToolCalls, tc.dispatched)
			}
		})
	}
}

// ---- the two token budgets ------------------------------------------------

func TestTheTokenBudgetIsCheckedBeforeTheCallThatWouldBlowIt(t *testing.T) {
	f := llm.NewFake(
		llm.Turn{Calls: toolTurn(0).Calls, Usage: llm.Usage{InputTokens: 4900, OutputTokens: 10}},
		finalTurn("never reached"),
	)
	b := testBounds()
	b.MaxInputTokens = 5000
	a := Run(context.Background(), f, okTools(), b, "q")
	// The recorder is the assertion: the trace's own Steps would read the same
	// under a check that ran one call too late.
	assertLoop(t, a, f, StopTokenBudget, 1, 1)
}

func TestBothTokenBudgetsAreCumulativeAcrossTheWholeLoop(t *testing.T) {
	turns := make([]llm.Turn, 0, 6)
	for i := range 6 {
		turns = append(turns, llm.Turn{Calls: toolTurn(i).Calls, Usage: llm.Usage{InputTokens: 5000, OutputTokens: 10}})
	}
	f := llm.NewFake(turns...)
	b := testBounds()
	b.MaxInputTokens = 12000
	a := Run(context.Background(), f, okTools(), b, "q")
	// Cumulative: 5,000 x 3 crosses 12,000. Under a per-call reading it runs to
	// MaxSteps and the published worst case is six times too large.
	assertLoop(t, a, f, StopTokenBudget, 3, 3)
	if a.Trace.Usage.InputTokens != 15000 {
		t.Errorf("trace usage input %d, want 15000", a.Trace.Usage.InputTokens)
	}
}

func TestTheOutputBudgetStopsTheLoopWhenTheRunningTotalReachesIt(t *testing.T) {
	turns := make([]llm.Turn, 0, 4)
	for i := range 3 {
		turns = append(turns, llm.Turn{Calls: toolTurn(i).Calls, Usage: llm.Usage{InputTokens: 10, OutputTokens: 600}})
	}
	turns = append(turns, llm.Turn{Text: "done", Usage: llm.Usage{InputTokens: 10, OutputTokens: 600}})
	f := llm.NewFake(turns...)
	b := testBounds()
	b.MaxOutputTokens = 1500
	a := Run(context.Background(), f, okTools(), b, "q")
	assertLoop(t, a, f, StopTokenBudget, 3, 3)
}

func TestEveryRequestCarriesTheRemainingOutputBudgetAsMaxTokens(t *testing.T) {
	f := llm.NewFake(
		llm.Turn{Calls: toolTurn(0).Calls, Usage: llm.Usage{InputTokens: 10, OutputTokens: 600}},
		finalTurn("done"),
	)
	b := testBounds()
	b.MaxOutputTokens = 1500
	Run(context.Background(), f, okTools(), b, "q")
	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(reqs))
	}
	// Unkillable without the recorder: the trace's Usage reports what the fake
	// chose to return, never what the loop asked for.
	if reqs[0].MaxTokens != 1500 {
		t.Errorf("request 0 carried MaxTokens %d, want 1500", reqs[0].MaxTokens)
	}
	if reqs[1].MaxTokens != 900 {
		t.Errorf("request 1 carried MaxTokens %d, want 900", reqs[1].MaxTokens)
	}
}

func TestAProviderTruncationIsNotServedAsAnAnswer(t *testing.T) {
	f := llm.NewFake(llm.Turn{Text: "half a sen", Stop: llm.StopMaxTokens})
	a := Run(context.Background(), f, okTools(), testBounds(), "q")
	assertLoop(t, a, f, StopTokenBudget, 1, 1)
	if a.Text != "" {
		t.Errorf("served a truncated answer %q", a.Text)
	}
}

// ---- the deadline ---------------------------------------------------------

// slowModel calls the inner model, then sleeps. The order matters: the request
// is recorded before the sleep, so the recorder counts calls that were made
// rather than calls that finished.
type slowModel struct {
	inner *llm.Fake
	d     time.Duration
}

func (s *slowModel) Name() string { return s.inner.Name() }

func (s *slowModel) Complete(ctx context.Context, r llm.Request) (llm.Response, error) {
	resp, err := s.inner.Complete(ctx, r)
	time.Sleep(s.d)
	return resp, err
}

// The parent is context.Background(). Under a shorter parent this mutation is a
// semantic no-op and the kill would be void — the same shape as clone.Run's
// inner deadline under the indexer's jobCtx.
func TestTheLoopDeadlineIsOneBudgetForTheWholeLoop(t *testing.T) {
	f := llm.NewFake(toolTurn(0), toolTurn(1), finalTurn("done"))
	b := testBounds()
	b.Deadline = 500 * time.Millisecond
	// 500 against 300 is an order of magnitude above a loaded runner's
	// scheduler noise; 50 against 30 was an earlier draft and is a flaky test.
	a := Run(context.Background(), &slowModel{inner: f, d: 300 * time.Millisecond}, okTools(), b, "q")
	assertLoop(t, a, f, StopDeadline, 2, 2)
}

// ---- tools ----------------------------------------------------------------

func TestAToolNotFoundIsDataForTheModelAndNotAStop(t *testing.T) {
	ts := &tools{fn: func(n int, _ llm.ToolCall) (Result, error) {
		if n == 1 {
			return Result{Content: `{"error":"not_found"}`, NotFound: true}, nil
		}
		return Result{Content: `{"ok":true}`, Read: []models.Span{{ID: "elsewhere"}}}, nil
	}}
	// Miss, then a read that succeeds, then an answer: the model routed around
	// the not-found, which is what makes it data rather than a stop.
	f := llm.NewFake(toolTurn(0), toolTurn(1), finalTurn("found it elsewhere [1]"))
	a := Run(context.Background(), f, ts, testBounds(), "q")
	assertLoop(t, a, f, StopFinal, 3, 3)
	if a.Text == "" {
		t.Errorf("answer was empty")
	}
	if ids := a.ReadIDs(); len(ids) != 1 || ids[0] != "elsewhere" {
		t.Errorf("Read = %v, want [elsewhere]: a not-found opens nothing", ids)
	}
	// The model is TOLD it was a not-found. Result.NotFound is what sets
	// ToolResult.IsError, and the provider wire format carries it — without it
	// the model reads an error body as an ordinary answer. Found by the
	// whole-branch sweep: dropping the field survived everything.
	reqs := f.Requests()
	if len(reqs) < 2 {
		t.Fatalf("recorded %d requests", len(reqs))
	}
	var sawError bool
	for _, m := range reqs[len(reqs)-1].Messages {
		for _, r := range m.Results {
			if r.IsError {
				sawError = true
			}
		}
	}
	if !sawError {
		t.Errorf("no tool result was marked IsError, so the model cannot tell a miss from an answer")
	}
}

func TestAToolStoreErrorStopsTheLoopRatherThanLettingItRetry(t *testing.T) {
	ts := &tools{fn: func(n int, _ llm.ToolCall) (Result, error) {
		if n == 2 {
			return Result{}, errors.New("pool exhausted")
		}
		return Result{Content: `{"ok":true}`}, nil
	}}
	f := llm.NewFake(toolTurns(4)...)
	a := Run(context.Background(), f, ts, testBounds(), "q")
	assertLoop(t, a, f, StopToolError, 2, 2)
}

func TestAnUnknownToolNameIsAMalformedCallAndNotAToolError(t *testing.T) {
	f := llm.NewFake(llm.Turn{Calls: []llm.ToolCall{{ID: "x", Name: "delete_repo", Args: json.RawMessage(`{}`)}}})
	ts := okTools()
	b := testBounds()
	b.MaxToolErrors = 0
	a := Run(context.Background(), f, ts, b, "q")
	assertLoop(t, a, f, StopMalformedToolCall, 1, 1)
	if a.Trace.Stop == StopToolError {
		t.Errorf("a confused model was filed as a failing store")
	}
	// Never dispatched: the loop refuses a name it was not told about rather
	// than handing it to the tool set.
	if len(ts.calls) != 0 {
		t.Errorf("the tool set was called %d times for an unknown tool, want 0", len(ts.calls))
	}
}

// N-1, N and N+1 on MaxToolErrors, with a program that recovers.
func TestAMalformedToolCallIsCorrectableTwiceAndThenStops(t *testing.T) {
	bad := func(_ int, c llm.ToolCall) (Result, error) {
		if strings.Contains(string(c.Args), `"bad":true`) {
			return Result{}, fmt.Errorf("%w: unknown key", ErrMalformedCall)
		}
		return Result{Content: `{"ok":true}`, Read: []models.Span{{ID: "recovered"}}}, nil
	}
	badTurn := func(i int) llm.Turn {
		return llm.Turn{Calls: []llm.ToolCall{{ID: fmt.Sprint(i), Name: "search_code", Args: json.RawMessage(fmt.Sprintf(`{"bad":true,"n":%d}`, i))}}}
	}
	for _, tc := range []struct {
		maxErrors int
		stop      Stop
		steps     int
	}{
		{1, StopMalformedToolCall, 2},
		{2, StopMalformedToolCall, 3},
		{3, StopFinal, 5},
	} {
		t.Run(fmt.Sprint(tc.maxErrors), func(t *testing.T) {
			f := llm.NewFake(badTurn(1), badTurn(2), badTurn(3), toolTurn(9), finalTurn("done [1]"))
			b := testBounds()
			b.MaxToolErrors = tc.maxErrors
			a := Run(context.Background(), f, &tools{fn: bad}, b, "q")
			assertLoop(t, a, f, tc.stop, tc.steps, tc.steps)
		})
	}
}

// N-1, N and N+1 on MaxRepeats, byte-identical calls.
func TestARepeatedIdenticalCallIsAnsweredOnceThenStops(t *testing.T) {
	same := readTurn("c", "read-a")
	for _, tc := range []struct {
		maxRepeats int
		stop       Stop
		steps      int
		dispatched int
	}{
		{0, StopRepeatedToolCall, 2, 1},
		{1, StopRepeatedToolCall, 3, 1},
		{2, StopRepeatedToolCall, 4, 1},
		{3, StopFinal, 5, 1},
	} {
		t.Run(fmt.Sprint(tc.maxRepeats), func(t *testing.T) {
			f := llm.NewFake(same, same, same, same, finalTurn("done [1]"))
			ts := readTools()
			b := testBounds()
			b.MaxRepeats = tc.maxRepeats
			b.MaxToolErrors = 5
			a := Run(context.Background(), f, ts, b, "q")
			assertLoop(t, a, f, tc.stop, tc.steps, tc.steps)
			// Answered once: the store is asked exactly once however many times
			// the model asks.
			if len(ts.calls) != tc.dispatched {
				t.Errorf("the tool set was called %d times, want %d", len(ts.calls), tc.dispatched)
			}
		})
	}
}

func TestEveryProviderFailureKindBecomesItsOwnStop(t *testing.T) {
	for _, k := range llm.Kinds {
		t.Run(string(k), func(t *testing.T) {
			f := llm.NewFake(llm.Turn{Err: &llm.Failure{Kind: k, Status: 500}})
			a := Run(context.Background(), f, okTools(), testBounds(), "q")
			assertLoop(t, a, f, StopOf(k), 1, 1)
		})
	}
	// A bug in our own code is not an outage: it must not land in the
	// provider-failure label an operator pages on.
	f := llm.NewFake(llm.Turn{Err: errors.New("nil map")})
	a := Run(context.Background(), f, okTools(), testBounds(), "q")
	if a.Trace.Stop == StopOf(llm.KindUnavailable) {
		t.Errorf("a non-provider error was filed as %s", StopOf(llm.KindUnavailable))
	}
	assertLoop(t, a, f, StopMalformedResponse, 1, 1)
}

func TestTheTraceNeverCarriesToolArguments(t *testing.T) {
	const canary = "CANARY-QUESTION-TEXT"
	f := llm.NewFake(
		llm.Turn{Calls: []llm.ToolCall{{ID: "s", Name: "search_code", Args: json.RawMessage(`{"q":"` + canary + `"}`)}}},
		finalTurn("done [1]"),
	)
	a := Run(context.Background(), f, okTools(), testBounds(), canary)
	b, err := json.Marshal(a.Trace)
	if err != nil {
		t.Fatalf("marshal trace: %v", err)
	}
	if strings.Contains(string(b), canary) {
		t.Errorf("the marshalled trace contains %q: %s", canary, b)
	}
	if len(a.Trace.Tools) != 1 || a.Trace.Tools[0].Name != "search_code" {
		t.Errorf("trace.tools = %v, want one search_code", a.Trace.Tools)
	}
}

// Read is the concatenation, in order, of what each call opened.
//
// The obvious fixture — four distinct spans, asserted against their insertion
// order — is a PROBABILISTIC kill against the map-built mutant, and this plan's
// own rule 7 forbids one. Measured on go1.27 with the swiss map: a four-entry
// map iterates in insertion order 62% of the time, and even at eight entries it
// coincides 12% of the time. So the load-bearing half of this fixture is the
// REPEATED span: a map keyed on span id deduplicates, which is wrong whatever
// order it iterates in, and the length assertion fails every run.
//
// The third turn opens read-a again through a different argument. It used to
// open it by spelling the SAME argument with a space in it, which the
// byte-identical repeat guard let through; the guard now normalises, so a
// fixture built on spelling would silently stop repeating anything and this
// test would go on passing while asserting nothing. What the loop actually
// promises is that it concatenates whatever each call reported reading, which
// is what the stub below exercises.
func TestReadIsOrderedSoAMarkerResolvesInAFixedOrder(t *testing.T) {
	aliased := &tools{fn: func(_ int, c llm.ToolCall) (Result, error) {
		var a struct {
			SpanID string `json:"span_id"`
		}
		_ = json.Unmarshal(c.Args, &a)
		id := a.SpanID
		if id == "read-a-again" {
			id = "read-a"
		}
		return Result{Content: `{"span":"` + id + `"}`, Read: []models.Span{{ID: id, Text: "t-" + id}}}, nil
	}}
	f := llm.NewFake(
		readTurn("1", "read-a"), readTurn("2", "read-b"), readTurn("3", "read-a-again"),
		finalTurn("done [1][3]"),
	)
	a := Run(context.Background(), f, aliased, testBounds(), "q")
	want := []string{"read-a", "read-b", "read-a"}
	if !reflect.DeepEqual(a.ReadIDs(), want) {
		t.Errorf("Read = %v, want %v", a.ReadIDs(), want)
	}

	// And the ordering claim itself, over four distinct spans.
	f2 := llm.NewFake(
		readTurn("1", "read-d"), readTurn("2", "read-c"),
		readTurn("3", "read-b"), readTurn("4", "read-a"),
		finalTurn("done [1][4]"),
	)
	a2 := Run(context.Background(), f2, readTools(), testBounds(), "q")
	// Insertion order is deliberately the reverse of sorted order, so an
	// implementation that sorted rather than appended also fails.
	if want := []string{"read-d", "read-c", "read-b", "read-a"}; !reflect.DeepEqual(a2.ReadIDs(), want) {
		t.Errorf("Read = %v, want %v", a2.ReadIDs(), want)
	}
}

func TestTheLoopMakesNoModelCallWhenItsContextIsAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := llm.NewFake(finalTurn("never"))
	a := Run(ctx, f, okTools(), testBounds(), "q")
	assertLoop(t, a, f, StopDeadline, 0, 0)
}

// The system prompt is the SECOND place the untrusted-content instruction
// lives, and the frame is the first. Both are mitigation rather than
// enforcement — a model can be persuaded by text inside a frame however it is
// labelled — and both are pinned by a string comparison, which is the honest
// form for a property with nothing behavioural to assert.
//
// Found by the whole-branch sweep: deleting the sentence from the system prompt
// survived everything, because only the frame's copy was tested.
func TestTheSystemPromptSaysToolResultsAreNotInstructions(t *testing.T) {
	for _, want := range []string{
		"unknown third party",
		"never an instruction",
		// The citation contract, which is what makes Resolve's positional rule
		// something the model was actually told.
		"read_span",
		"[n]",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("the system prompt does not say %q:\n%s", want, systemPrompt)
		}
	}
	// And it carries no repository text and no question: it is a compile-time
	// constant, which is what makes that true rather than hoped for.
	if strings.Contains(systemPrompt, "%s") || strings.Contains(systemPrompt, "%v") {
		t.Errorf("the system prompt has a format verb in it, so something is interpolated into it")
	}
}

// A loop that called no tool still has to say so in the shape every other loop
// says it in. Before this, tools was grown from nil by append and marshalled as
// null on exactly the loops that did least — which is the same field the
// gateway serves for busy and budget_exhausted.
func TestATraceFromALoopThatCalledNoToolSerialisesToolsAsAnEmptyList(t *testing.T) {
	f := llm.NewFake(llm.Turn{Text: "no tool, no citation"})
	a := Run(context.Background(), f, readTools(), testBounds(), "q")
	if a.Trace.Stop != StopUncited {
		t.Fatalf("stop %q, want %q", a.Trace.Stop, StopUncited)
	}
	b, err := json.Marshal(a.Trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"tools":[]`) {
		t.Errorf("trace marshalled as %s, want tools []", b)
	}
}

// The repeat guard keyed on bytes, so {"span_id":"x"} and {"span_id": "x"} were
// two calls. Both spellings are ordinary model output, and the second one ran
// read_span again: the character budget charged twice for one span, and that
// span at two positions in Read — from which two markers resolve to two
// citations carrying one span_id, which is the id a console keys its list on.
func TestOneCallSpelledTwoWaysIsOneCall(t *testing.T) {
	spaced := llm.Turn{Calls: []llm.ToolCall{{ID: "2", Name: "read_span",
		Args: json.RawMessage(`{"span_id": "read-a"}`)}}}
	f := llm.NewFake(readTurn("1", "read-a"), spaced, finalTurn("first [1] and second [2]"))
	a := Run(context.Background(), f, readTools(), testBounds(), "q")

	if got := a.ReadIDs(); !reflect.DeepEqual(got, []string{"read-a"}) {
		t.Fatalf("Read = %v, want one read-a: the same call spelled twice opened the span twice", got)
	}
	// The consequence, asserted rather than inferred: [2] now resolves to
	// nothing and is dropped, where before it resolved to the duplicate.
	if len(a.Cited) != 1 || a.Trace.CitationsDropped != 1 {
		t.Errorf("cited %v with %d dropped, want one citation and one drop",
			a.Cited, a.Trace.CitationsDropped)
	}
	seen := map[string]bool{}
	for _, pos := range a.Cited {
		id := a.Read[pos].ID
		if seen[id] {
			t.Errorf("two citations carry span_id %q", id)
		}
		seen[id] = true
	}
}

// The key itself, because the loop can only show whitespace: object key order
// and the fallback for arguments that are not JSON at all have no turn that
// produces them.
func TestTheRepeatKeyIsTheCallAndNotItsSpelling(t *testing.T) {
	call := func(name, args string) llm.ToolCall {
		return llm.ToolCall{ID: "x", Name: name, Args: json.RawMessage(args)}
	}
	for name, pair := range map[string][2]llm.ToolCall{
		"a space after the colon": {call("read_span", `{"span_id":"x"}`), call("read_span", `{"span_id": "x"}`)},
		"pretty-printed":          {call("search_code", `{"q":"Get"}`), call("search_code", "{\n  \"q\": \"Get\"\n}")},
		"keys in another order": {call("callers_of", `{"symbol_id":"s","depth":2}`),
			call("callers_of", `{"depth":2,"symbol_id":"s"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if repeatKey(pair[0]) != repeatKey(pair[1]) {
				t.Errorf("%q and %q key differently", pair[0].Args, pair[1].Args)
			}
		})
	}
	// And what must stay apart. The last pair is the fallback: arguments this
	// cannot parse key on their own bytes, which is the behaviour that shipped
	// — a normaliser that collapsed them would be the second place to be wrong
	// the byte-identical key was written to avoid.
	for name, pair := range map[string][2]llm.ToolCall{
		"a different value":           {call("read_span", `{"span_id":"x"}`), call("read_span", `{"span_id":"y"}`)},
		"a different tool":            {call("read_span", `{"span_id":"x"}`), call("definition_of", `{"span_id":"x"}`)},
		"two large integers":          {call("callers_of", `{"n":12345678901234567890}`), call("callers_of", `{"n":12345678901234567891}`)},
		"arguments that are not JSON": {call("read_span", `not json`), call("read_span", `also not json`)},
	} {
		t.Run(name, func(t *testing.T) {
			if repeatKey(pair[0]) == repeatKey(pair[1]) {
				t.Errorf("%q and %q key the same", pair[0].Args, pair[1].Args)
			}
		})
	}
}
