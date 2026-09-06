package agent

import (
	"github.com/mralaminahamed/codetrail/packages/shared/llm"
)

// Stop names why the loop ended. It is a closed set, a metric label and a
// response field, so a tenth reason cannot be added without a series and a
// payload key.
type Stop string

const (
	// StopFinal is the only one that is not a degradation.
	StopFinal Stop = "final"

	StopStepLimit         Stop = "step_limit"
	StopToolCallLimit     Stop = "tool_call_limit"
	StopTokenBudget       Stop = "token_budget"
	StopDeadline          Stop = "deadline"
	StopMalformedToolCall Stop = "malformed_tool_call"
	StopToolError         Stop = "tool_error"
	StopRepeatedToolCall  Stop = "repeated_tool_call"
	StopUncited           Stop = "uncited"

	// StopMalformedResponse is llm.KindMalformed's stop, written as a
	// conversion of that constant so the two spellings cannot drift.
	StopMalformedResponse = Stop(llm.KindMalformed)

	// The gateway's two, decided before Run is ever called. They live here so
	// the vocabulary is one list rather than two that drift.
	StopBusy            Stop = "busy"
	StopBudgetExhausted Stop = "budget_exhausted"
)

// StopOf maps a provider failure to its stop. Same spelling both sides, which
// is what makes Stops derivable from llm.Kinds rather than written twice.
func StopOf(k llm.Kind) Stop { return Stop(k) }

// Stops is the whole vocabulary, with the provider kinds derived rather than
// listed: a kind added to llm with no Stop here is a degradation that reports
// nothing, and Task 5's metric labels read this list.
var Stops = func() []Stop {
	out := []Stop{
		StopFinal, StopStepLimit, StopToolCallLimit, StopTokenBudget, StopDeadline,
		StopMalformedToolCall, StopToolError, StopRepeatedToolCall, StopUncited,
		StopBusy, StopBudgetExhausted,
	}
	seen := make(map[Stop]bool, len(out))
	for _, s := range out {
		seen[s] = true
	}
	for _, k := range llm.Kinds {
		if s := StopOf(k); !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}()

func (s Stop) String() string { return string(s) }

// ToolInvocation is one tool call as a reader sees it: which tool, and how long
// it took.
//
// Never the arguments. An argument to search_code is the model's rewriting of
// the user's question, and the rule that keeps a question out of a log
// (read_test.go's TestTheQuestionIsNeverLogged) extends to a payload a console
// renders into an operator's screenshot.
type ToolInvocation struct {
	Name string `json:"name"`
	MS   int64  `json:"ms"`
}

// Trace is what the loop did, on the wire in full.
//
// It exists because a bound whose only effect is fewer iterations is
// unkillable. Steps says where it stopped, Stop says which ceiling, and Tools
// is an ordered list a test can compare element by element — three observers
// for every ceiling, beside the fake's own request recorder.
// NewTrace starts one, and is the only place a Trace is built: the gateway
// makes one of its own for the two degradations decided before Run is called,
// and a Tools grown from nil by append serialises as null on exactly those —
// busy and budget_exhausted, the loops that called no tool. One empty list
// spelled two ways is one a client has to guess about.
func NewTrace(model string) Trace {
	return Trace{Model: model, Tools: []ToolInvocation{}}
}

type Trace struct {
	Model            string           `json:"model"`
	Steps            int              `json:"steps"`
	ToolCalls        int              `json:"tool_calls"`
	Stop             Stop             `json:"stop"`
	Tools            []ToolInvocation `json:"tools"`
	Usage            llm.Usage        `json:"usage"`
	CitationsDropped int              `json:"citations_dropped"`
}
