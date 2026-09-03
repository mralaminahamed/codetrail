package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/llm"
)

// Every default is pinned, because the README publishes all seven as the
// bounds an operator is running under. A test that pinned only the arithmetic
// would let any of the other five move with nothing to notice.
func TestDefaultBoundsValidateAndPublishTheWorstCase(t *testing.T) {
	b := DefaultBounds()
	if err := b.Validate(); err != nil {
		t.Fatalf("DefaultBounds does not validate: %v", err)
	}
	want := Bounds{
		MaxSteps: 6, MaxToolCalls: 12, MaxToolErrors: 2, MaxRepeats: 2,
		MaxInputTokens: 12000, MaxOutputTokens: 1500, Deadline: 60 * time.Second,
	}
	if b != want {
		t.Errorf("DefaultBounds() = %+v, want %+v", b, want)
	}
	if perCallOutputCap != 1500 {
		t.Errorf("perCallOutputCap = %d, want 1500", perCallOutputCap)
	}
	// The number the README publishes. Both budgets are cumulative totals for
	// the whole loop, so the worst case is their sum and not MaxSteps times
	// either of them.
	if got := b.MaxInputTokens + b.MaxOutputTokens; got != 13500 {
		t.Errorf("per-request worst case = %d tokens, want 13500", got)
	}
	if b.MaxOutputTokens > perCallOutputCap*b.MaxSteps {
		t.Errorf("MaxOutputTokens %d is unreachable under a per-call cap of %d over %d steps",
			b.MaxOutputTokens, perCallOutputCap, b.MaxSteps)
	}
}

func TestValidateRefusesEveryUnservableBound(t *testing.T) {
	for name, mut := range map[string]func(*Bounds){
		"MaxSteps":        func(b *Bounds) { b.MaxSteps = 0 },
		"MaxToolCalls":    func(b *Bounds) { b.MaxToolCalls = 0 },
		"MaxToolErrors":   func(b *Bounds) { b.MaxToolErrors = -1 },
		"MaxRepeats":      func(b *Bounds) { b.MaxRepeats = -1 },
		"MaxInputTokens":  func(b *Bounds) { b.MaxInputTokens = 0 },
		"MaxOutputTokens": func(b *Bounds) { b.MaxOutputTokens = 0 },
		"Deadline":        func(b *Bounds) { b.Deadline = 0 },
	} {
		b := DefaultBounds()
		mut(&b)
		err := b.Validate()
		if err == nil {
			t.Errorf("%s: Validate accepted it", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error %q does not name the field", name, err)
		}
	}
}

// A range over a mutated Stops is vacuous, so the length is asserted against a
// hand-written count as well.
func TestStopsIsClosedAndCoversEveryLlmKind(t *testing.T) {
	have := map[Stop]bool{}
	for _, s := range Stops {
		if have[s] {
			t.Errorf("Stops carries %q twice", s)
		}
		have[s] = true
	}
	for _, k := range llm.Kinds {
		if !have[StopOf(k)] {
			t.Errorf("llm kind %q has no Stop", k)
		}
	}
	// 9 loop stops + 2 gateway stops + the 4 llm kinds that are not already
	// spelled by one of those (deadline is).
	if len(Stops) != 15 {
		t.Errorf("Stops has %d entries, want 15: %v", len(Stops), Stops)
	}
	for _, s := range []Stop{
		StopFinal, StopStepLimit, StopToolCallLimit, StopTokenBudget, StopDeadline,
		StopMalformedToolCall, StopToolError, StopRepeatedToolCall, StopUncited,
		StopBusy, StopBudgetExhausted, StopMalformedResponse,
	} {
		if !have[s] {
			t.Errorf("Stops is missing %q", s)
		}
		if s.String() == "" {
			t.Errorf("%v has no string form", s)
		}
	}
	// A provider kind and a tool fault must never share a label: one is a
	// broken provider, the other a confused model.
	if StopMalformedResponse == StopMalformedToolCall {
		t.Errorf("malformed_response and malformed_tool_call are the same label")
	}
}

func TestTheLoopDeadlineIsAPositiveDefault(t *testing.T) {
	if DefaultBounds().Deadline < time.Second {
		t.Errorf("Deadline = %v", DefaultBounds().Deadline)
	}
}
