package agent

import (
	"fmt"
	"time"
)

// Bounds is every ceiling the loop has. spec:234 says "bounded" and names no
// bound; these are the bounds, and each one has a stop reason of its own so a
// reader of a trace can tell which fired.
//
// MaxInputTokens and MaxOutputTokens are CUMULATIVE TOTALS FOR THE WHOLE LOOP,
// not per-call limits. A per-call cap bounds nothing — six steps under a
// per-call cap of 12,000 can spend 72,000 and never trip it — and the published
// per-request worst case is MaxInputTokens + MaxOutputTokens for exactly this
// reason.
type Bounds struct {
	MaxSteps        int
	MaxToolCalls    int
	MaxToolErrors   int
	MaxRepeats      int
	MaxInputTokens  int
	MaxOutputTokens int
	Deadline        time.Duration
}

// perCallOutputCap is what Request.MaxTokens is clamped to on every call. A
// constant rather than a knob: it is the only mechanism that can stop a single
// call running away, and two knobs whose interaction nobody has measured is a
// configuration surface with no consumer.
const perCallOutputCap = 1500

// DefaultBounds. None of these numbers is measured, and none pretends to be:
// they are chosen to be obviously finite, the way rag.DefaultFloor's -1 is
// chosen to be obviously not a threshold. The deadline is one budget for the
// whole loop and has to cover up to MaxToolCalls embedder round trips, each of
// which the gateway bounds at 15s of its own.
func DefaultBounds() Bounds {
	return Bounds{
		MaxSteps:        6,
		MaxToolCalls:    12,
		MaxToolErrors:   2,
		MaxRepeats:      2,
		MaxInputTokens:  12000,
		MaxOutputTokens: 1500,
		Deadline:        60 * time.Second,
	}
}

// Validate fails closed like chunk.Options and rag.Floor. It runs at boot, not
// per request: an invalid Bounds is a programming fault, and Run's signature
// deliberately has no error for the caller to propagate into a 500.
func (b Bounds) Validate() error {
	switch {
	case b.MaxSteps < 1:
		return fmt.Errorf("agent: MaxSteps must be positive, got %d", b.MaxSteps)
	case b.MaxToolCalls < 1:
		return fmt.Errorf("agent: MaxToolCalls must be positive, got %d", b.MaxToolCalls)
	case b.MaxToolErrors < 0:
		return fmt.Errorf("agent: MaxToolErrors must not be negative, got %d", b.MaxToolErrors)
	case b.MaxRepeats < 0:
		return fmt.Errorf("agent: MaxRepeats must not be negative, got %d", b.MaxRepeats)
	case b.MaxInputTokens < 1:
		return fmt.Errorf("agent: MaxInputTokens must be positive, got %d", b.MaxInputTokens)
	case b.MaxOutputTokens < 1:
		return fmt.Errorf("agent: MaxOutputTokens must be positive, got %d", b.MaxOutputTokens)
	case b.Deadline <= 0:
		return fmt.Errorf("agent: Deadline must be positive, got %v", b.Deadline)
	}
	return nil
}
