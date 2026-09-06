package llm

import (
	"context"
	"fmt"
	"sync"
)

// Turn is one scripted answer. Err makes every degradation drivable from a
// test: a turn carrying a *Failure is how the five provider kinds get their own
// branches without a server.
type Turn struct {
	Text  string
	Calls []ToolCall
	Usage Usage
	Stop  string
	Err   error
}

// Fake is a scripted program with a recorder.
//
// Deterministic by construction, the way embed.Fake states it: no clock, no map
// iteration, no math/rand. Turn i answers call i.
//
// Requests is the load-bearing half. It is the only observer that measures work
// actually done rather than what was reported, which is what makes a ceiling
// killable at all — a mutant that raises MaxSteps and still writes the right
// trace is invisible to every assertion but this one.
type Fake struct {
	mu    sync.Mutex
	turns []Turn
	n     int
	reqs  []Request
}

func NewFake(turns ...Turn) *Fake { return &Fake{turns: turns} }

// Name is what the trace records, so a corpus of traces says which model
// answered rather than leaving a reader to remember.
func (f *Fake) Name() string { return "fake-scripted" }

func (f *Fake) Complete(ctx context.Context, r Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, &Failure{Kind: KindDeadline, Detail: err.Error()}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.n >= len(f.turns) {
		// Loudly, not with a zero Response: a zero Response with a nil error is
		// indistinguishable from a model that finished, so a test that scripted
		// too few turns would pass while measuring nothing. embed.Fake refuses a
		// text it can hash no token from for the same reason.
		return Response{}, &Failure{
			Kind:   KindMalformed,
			Detail: fmt.Sprintf("fake: program exhausted after %d turns", len(f.turns)),
		}
	}
	t := f.turns[f.n]
	f.n++
	if t.Err != nil {
		return Response{}, t.Err
	}
	u := t.Usage
	if u == (Usage{}) {
		// A turn that says nothing about tokens is estimated, which is what the
		// provider client does when a provider reports nothing. A scripted zero
		// would make the budget free and every budget test vacuous.
		u = EstimatedUsage(promptOf(r), t.Text)
	}
	return Response{Text: t.Text, Calls: t.Calls, Usage: u, Stop: stopOf(t)}, nil
}

// Requests is every Request this fake received, in order, verbatim.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Request, len(f.reqs))
	copy(out, f.reqs)
	return out
}

func stopOf(t Turn) string {
	if t.Stop != "" {
		return t.Stop
	}
	if len(t.Calls) > 0 {
		return "tool_use"
	}
	return "end_turn"
}

// promptOf is what the estimator sizes: everything the request would send.
func promptOf(r Request) string {
	s := r.System
	for _, m := range r.Messages {
		s += m.Text
		for _, res := range m.Results {
			s += res.Content
		}
	}
	return s
}
