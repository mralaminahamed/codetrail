// Package llm is the seam between the answering loop and whatever generates
// text.
//
// Shaped like packages/shared/embed, and for the same reason its doc gives: the
// interface has two implementations from the start because CI has to prove the
// mechanics — turn taking, tool dispatch, every ceiling, every degradation —
// with no model and no network, or they are only ever exercised on a
// developer's machine. Here the argument is sharper, because a model call also
// costs money and spec:230 makes the free path the default.
//
// Nothing in this file dials anything. The provider client is anthropic.go and
// is constructed only by FromEnv.
package llm

import (
	"context"
	"encoding/json"
)

// Model is one generation, and an identity for the trace.
//
// Name, not Model: this interface's own type is called Model, so Model.Model()
// would be unreadable. The value lands in the trace's llm.model field, which is
// what makes answered_by:"llm" actionable — "which llm" is a different question
// from "an llm", the same reason embed.Fake names itself in embed_model.
//
// Complete returns a Response for a model that answered and a *Failure for a
// provider that did not. It never retries: the loop owns the one deadline and
// the step ceiling, and a retry inside here is spend the trace cannot see.
type Model interface {
	Name() string
	Complete(ctx context.Context, r Request) (Response, error)
}

// Role is closed at two. The system prompt is a field on Request rather than a
// third role because the provider protocol treats it as one, and a third value
// here would be a role no request could carry.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Request is one turn's input. MaxTokens is the per-call output ceiling the
// loop sets on every call; it is the only mechanism that can bound the call
// that is already running, since a cumulative check happens between calls.
type Request struct {
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
}

// Message is one turn of the transcript. A user message carries either Text or
// Results; an assistant message carries Text and Calls.
type Message struct {
	Role    Role
	Text    string
	Calls   []ToolCall
	Results []ToolResult
}

type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolCall is what the model asked for. Args stays raw so the tool decodes it
// with its own rules — DisallowUnknownFields among them — rather than this
// package inventing a second decoder that would disagree.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Usage carries whether the counts came from the provider or from us.
//
// A token budget built on numbers we invented is not a bound on spend. The
// estimator is EstimateTokens and makes no pretence of being a tokeniser, so
// the budget's provenance is auditable rather than assumed.
type Usage struct {
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	Estimated    bool `json:"estimated"`
}

// Response is what a model said. Stop is the provider's own stop reason and is
// read for exactly one thing: "max_tokens" means the per-call output cap
// truncated this generation, so the prose is a fragment and the loop must stop
// rather than serve half a sentence.
type Response struct {
	Text  string
	Calls []ToolCall
	Usage Usage
	Stop  string
}

// StopMaxTokens is the provider stop reason that means the answer was cut off
// at Request.MaxTokens.
const StopMaxTokens = "max_tokens"

// charsPerToken is the estimator's whole model of tokenisation. Four is the
// number every provider's own "rough guide" quotes for English prose and it is
// calibrated against nothing; Usage.Estimated is what stops a budget spent
// against it being read as measured.
const charsPerToken = 4

// EstimateTokens is len(s)/4, documented as a guess.
func EstimateTokens(s string) int { return len(s) / charsPerToken }

// EstimatedUsage is what a caller reports when the provider reported nothing.
// Estimated is true here and only here, so a reader of the budget can tell a
// number that came from a bill from one that came from a division.
func EstimatedUsage(in, out string) Usage {
	return Usage{InputTokens: EstimateTokens(in), OutputTokens: EstimateTokens(out), Estimated: true}
}

// EstimateRequest sizes a whole request the way the input budget has to size it
// before the call: everything that will be sent, system prompt and tool schemas
// included, because all of it is billed.
func EstimateRequest(r Request) int {
	n := EstimateTokens(r.System)
	for _, m := range r.Messages {
		n += EstimateTokens(m.Text)
		for _, c := range m.Calls {
			n += EstimateTokens(c.Name) + EstimateTokens(string(c.Args))
		}
		for _, res := range m.Results {
			n += EstimateTokens(res.Content)
		}
	}
	for _, t := range r.Tools {
		n += EstimateTokens(t.Name) + EstimateTokens(t.Description) + EstimateTokens(string(t.Schema))
	}
	return n
}
