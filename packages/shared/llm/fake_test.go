package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func call(name, args string) ToolCall {
	return ToolCall{ID: "c-" + name, Name: name, Args: json.RawMessage(args)}
}

// Two turns requesting *different* tools. A program of identical turns cannot
// tell a fixed index from an advancing one.
func TestAFakeReturnsItsTurnsInOrderAndRecordsEveryRequest(t *testing.T) {
	f := NewFake(
		Turn{Calls: []ToolCall{call("search_code", `{"q":"one"}`)}},
		Turn{Calls: []ToolCall{call("read_span", `{"span_id":"a"}`)}},
	)
	ctx := context.Background()

	r0, err := f.Complete(ctx, Request{System: "sys", Messages: []Message{{Role: RoleUser, Text: "first"}}})
	if err != nil {
		t.Fatalf("turn 0: %v", err)
	}
	if got := r0.Calls[0].Name; got != "search_code" {
		t.Errorf("turn 0: got call %q, want %q", got, "search_code")
	}
	r1, err := f.Complete(ctx, Request{System: "sys", Messages: []Message{{Role: RoleUser, Text: "first"}, {Role: RoleAssistant, Text: "second"}}})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if got := r1.Calls[0].Name; got != "read_span" {
		t.Errorf("turn 1: got call %q, want %q", got, "read_span")
	}

	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(reqs))
	}
	// The second request's transcript too, so a recorder that truncates also
	// fails rather than only one that records nothing.
	if len(reqs[1].Messages) != 2 || reqs[1].Messages[1].Text != "second" {
		t.Errorf("recorded request 1 as %+v, want its two messages verbatim", reqs[1].Messages)
	}

	// A COPY, not the live slice. The recorder is the observer every ceiling
	// test rests on, so a caller that sorted or truncated what it got back would
	// corrupt the evidence for every later assertion in the same test — and
	// nothing would say so. Found by the whole-branch sweep: returning f.reqs
	// directly survived everything.
	reqs[0] = Request{System: "clobbered"}
	if again := f.Requests(); again[0].System != "sys" {
		t.Errorf("mutating the returned slice reached the recorder: %+v", again[0])
	}
}

// One turn, driven twice.
func TestAFakeWhoseProgramIsExhaustedFailsRatherThanReturningZero(t *testing.T) {
	f := NewFake(Turn{Text: "done"})
	ctx := context.Background()
	if _, err := f.Complete(ctx, Request{}); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	resp, err := f.Complete(ctx, Request{})
	kind, ok := Classify(err)
	if !ok || kind != KindMalformed {
		t.Fatalf("call 2 returned (%+v, %v), want a %s failure", resp, err, KindMalformed)
	}
	if want := "fake: program exhausted after 1 turns"; err.Error() == "" || !strings.Contains(err.Error(), want) {
		t.Errorf("call 2 error %q does not say %q", err, want)
	}
}

func TestAFakeTurnCanCarryAnErrorSoEveryFailureModeIsScriptable(t *testing.T) {
	for _, k := range Kinds {
		f := NewFake(Turn{Err: &Failure{Kind: k, Status: 500}})
		_, err := f.Complete(context.Background(), Request{})
		got, ok := Classify(err)
		if !ok || got != k {
			t.Errorf("scripted %s, got (%s, %v)", k, got, ok)
		}
	}
}

func TestTheFakeNamesItselfSoATraceSaysWhichModelAnswered(t *testing.T) {
	if got := NewFake().Name(); got != "fake-scripted" {
		t.Errorf("Name() = %q, want %q", got, "fake-scripted")
	}
}

// Two responses: one whose usage came from the provider and one estimated. A
// fixture with only reported usage cannot detect a hard-wired Estimated.
func TestUsageSaysWhetherItsCountsWereReportedOrEstimated(t *testing.T) {
	reported := Usage{InputTokens: 9000, OutputTokens: 40}
	f := NewFake(
		Turn{Text: "reported", Usage: reported},
		Turn{Text: "0123456789012345678901234567890123456789"}, // 40 chars -> 10 tokens
	)
	ctx := context.Background()

	r0, err := f.Complete(ctx, Request{})
	if err != nil {
		t.Fatalf("turn 0: %v", err)
	}
	if r0.Usage.Estimated {
		t.Errorf("reported usage reported Estimated=true")
	}
	if r0.Usage.InputTokens != 9000 {
		t.Errorf("reported usage was rewritten: %+v", r0.Usage)
	}

	r1, err := f.Complete(ctx, Request{System: "01234567"}) // 8 chars -> 2 tokens
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if !r1.Usage.Estimated {
		t.Errorf("estimated usage reported Estimated=false")
	}
	if r1.Usage.InputTokens != 2 || r1.Usage.OutputTokens != 10 {
		t.Errorf("estimated usage = %+v, want {2 10 true}", r1.Usage)
	}
	if got := EstimatedUsage("abcd", "abcdefgh"); got != (Usage{InputTokens: 1, OutputTokens: 2, Estimated: true}) {
		t.Errorf("EstimatedUsage = %+v, want {1 2 true}", got)
	}
}

func TestTheFakeRefusesACallWhoseContextIsAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := NewFake(Turn{Text: "never"})
	_, err := f.Complete(ctx, Request{})
	if kind, ok := Classify(err); !ok || kind != KindDeadline {
		t.Fatalf("Complete on a cancelled context = %v, want a %s failure", err, KindDeadline)
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("recorded %d requests for a cancelled call, want 0", n)
	}
}

func TestEstimateRequestSizesEverythingThatWouldBeBilled(t *testing.T) {
	r := Request{
		System:   "0123",                                                                  // 1
		Messages: []Message{{Role: RoleUser, Text: "01234567"}},                           // 2
		Tools:    []ToolSpec{{Name: "0123", Description: "0123", Schema: []byte(`0123`)}}, // 3
	}
	if got := EstimateRequest(r); got != 6 {
		t.Errorf("EstimateRequest = %d, want 6", got)
	}
	// A tool result is repository text and is the largest thing in a late
	// request; a estimator that ignored it would under-report the budget by
	// most of what it is bounding.
	r.Messages = append(r.Messages, Message{Role: RoleUser, Results: []ToolResult{{Content: "0123456789012345"}}})
	if got := EstimateRequest(r); got != 10 {
		t.Errorf("EstimateRequest with a tool result = %d, want 10", got)
	}
}
