//go:build llm

package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The one suite in this project that costs money.
//
// The build tag keeps it out of CI on purpose, and CI vets it — `go vet
// -tags=llm ./...` — for the reason ci.yml already gives for the ollama tag: a
// tagged file is skipped by an untagged vet, so an API change here could break
// the one check a fake cannot make and stay green for as long as nobody ran it
// by hand.
//
// What the httptest tests cannot prove: that the request shape this client
// builds is the one the Messages API actually accepts. Everything else — the
// transport controls, the failure taxonomy, the redaction — is provable against
// a loopback socket, and is.
func liveKey(t *testing.T) Secret {
	t.Helper()
	s, err := LoadSecret()
	if err != nil {
		// A failure, not a skip. A skipped suite prints the same "ok" as one
		// that ran, and asking for this tag is already a statement that a key is
		// meant to be there.
		t.Fatal("no key: -tags=llm sends a PAID request to a real provider, and a skip here is a green run with no coverage")
	}
	return s
}

func TestAnthropicLiveAnswersAndReportsUsage(t *testing.T) {
	a, err := NewAnthropic(defaultBaseURL, "claude-opus-5", liveKey(t), 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Complete(context.Background(), Request{
		MaxTokens: 64,
		System:    "Answer in exactly one word.",
		Messages:  []Message{{Role: RoleUser, Text: "What colour is a ripe banana?"}},
	})
	if err != nil {
		t.Fatalf("live request failed: %v", err)
	}
	if strings.TrimSpace(got.Text) == "" {
		t.Errorf("the provider answered with no text: %+v", got)
	}
	// The half a fake cannot check: the provider reports its own counts, so the
	// budget is spending measured tokens rather than len(text)/4.
	if got.Usage.Estimated {
		t.Errorf("the provider reported no usage, so the budget fell back to an estimate: %+v", got.Usage)
	}
	if got.Usage.InputTokens <= 0 || got.Usage.OutputTokens <= 0 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if got.Stop == "" {
		t.Errorf("no stop reason: %+v", got)
	}
	t.Logf("live: stop=%q input=%d output=%d", got.Stop, got.Usage.InputTokens, got.Usage.OutputTokens)
}

// The tool-use round trip, which is the whole reason this client exists and the
// one shape a recorded fixture would let drift.
func TestAnthropicLiveEmitsAToolCallInTheShapeTheLoopExpects(t *testing.T) {
	a, err := NewAnthropic(defaultBaseURL, "claude-opus-5", liveKey(t), 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Complete(context.Background(), Request{
		MaxTokens: 256,
		System:    "Use the search_code tool to find where something is defined. Do not answer from memory.",
		Tools: []ToolSpec{{
			Name:        "search_code",
			Description: "Rank spans of this repository against a query.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`),
		}},
		Messages: []Message{{Role: RoleUser, Text: "Where is parseConfig defined?"}},
	})
	if err != nil {
		t.Fatalf("live request failed: %v", err)
	}
	if len(got.Calls) == 0 {
		t.Fatalf("no tool call: stop=%q text=%q", got.Stop, got.Text)
	}
	c := got.Calls[0]
	if c.ID == "" || c.Name != "search_code" {
		t.Errorf("tool call = %+v", c)
	}
	var args map[string]any
	if err := json.Unmarshal(c.Args, &args); err != nil {
		t.Errorf("tool call arguments are not an object: %v (%s)", err, c.Args)
	}
	t.Logf("live tool call: name=%s stop=%q input=%d output=%d",
		c.Name, got.Stop, got.Usage.InputTokens, got.Usage.OutputTokens)
}

// An obviously wrong key must classify as unauthorized and not as an outage:
// one needs a human, the other resolves itself.
func TestAnthropicLiveRejectsABadKeyAsUnauthorized(t *testing.T) {
	a, err := NewAnthropic(defaultBaseURL, "claude-opus-5", NewSecret("sk-ant-obviously-not-a-key"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Complete(context.Background(), Request{
		MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "hi"}},
	})
	if kind, ok := Classify(err); !ok || kind != KindUnauthorized {
		t.Errorf("a bad key classified as (%s, %v), want %s", kind, ok, KindUnauthorized)
	}
	if err != nil && strings.Contains(err.Error(), "sk-ant-obviously-not-a-key") {
		t.Errorf("the failure carried the key: %v", err)
	}
}
