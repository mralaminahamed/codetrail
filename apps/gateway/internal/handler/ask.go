package handler

import (
	"context"
	"sync"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/agent"
	"github.com/mralaminahamed/codetrail/packages/shared/llm"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// The two answerers, closed. answered_by is one of exactly these, on every
// /ask response including a refusal.
const (
	answererExtractive = "extractive"
	answererLLM        = "llm"
)

// Answerer is the bounded loop as the handler sees it.
//
// It returns no error, for the same reason agent.Run does not: every failure —
// a rate limit, a tool fault, a ceiling, a busy process, an exhausted budget —
// is an outcome the handler degrades from, and an error in the signature is an
// invitation to propagate it into a 500.
//
// An interface, so the handler's tests drive every degradation without a model.
type Answerer interface {
	Ask(ctx context.Context, repoID, q string, out rag.Result) agent.Answer
}

// Loop is the shipped Answerer: the model, the tools, the bounds, and the two
// per-process spend controls.
type Loop struct {
	Model  llm.Model
	Corpus agent.Corpus
	Bounds agent.Bounds
	Limits agent.ToolLimits

	// BelowFloor lets the loop run on a result the floor refused. Defaults to
	// false and ships that way: the floor is -1 and uncalibrated, so in the
	// shipped configuration this branch is unreachable, and shipping an
	// override of an uncalibrated mechanism is a guess wearing a feature's
	// clothes.
	BelowFloor bool

	sem    chan struct{}
	budget *budget
}

// NewLoop wires the spend controls. maxConcurrent and tokensPerHour are both
// PER PROCESS: with n replicas the real ceiling is n times either of them, and
// the README says so.
func NewLoop(m llm.Model, c agent.Corpus, b agent.Bounds, lim agent.ToolLimits, belowFloor bool,
	maxConcurrent, tokensPerHour int, now func() time.Time,
) *Loop {
	l := &Loop{Model: m, Corpus: c, Bounds: b, Limits: lim, BelowFloor: belowFloor,
		sem: make(chan struct{}, maxConcurrent), budget: newBudget(tokensPerHour, now)}
	metrics.SetLLMBudget(l.budget.remaining())
	return l
}

func (l *Loop) Ask(ctx context.Context, repoID, q string, _ rag.Result) agent.Answer {
	tr := agent.NewTrace(l.Model.Name())

	// Over the cap, DEGRADE rather than queue. A spend control that queues is a
	// latency control: the gateway has no request-timeout middleware, so a queue
	// has no bound, and a caller who waited 40s for a model is worse off than
	// one who got a cited extractive answer in 30ms.
	select {
	case l.sem <- struct{}{}:
		defer func() { <-l.sem }()
	default:
		tr.Stop = agent.StopBusy
		return agent.Answer{Trace: tr}
	}

	// Checked before the call, not after, for the reason the loop's own input
	// budget is: a bound checked after the spend is a report.
	if !l.budget.allow(l.Bounds.MaxInputTokens + l.Bounds.MaxOutputTokens) {
		tr.Stop = agent.StopBudgetExhausted
		return agent.Answer{Trace: tr}
	}

	// The tools are bound to this repository HERE, before the first model call,
	// from the id the handler took out of the URL path. No tool schema has a
	// repo field and nothing the model writes reaches this argument.
	a := agent.Run(ctx, l.Model, agent.NewTools(l.Corpus, repoID, l.Limits), l.Bounds, q)
	l.budget.spend(a.Trace.Usage.InputTokens + a.Trace.Usage.OutputTokens)
	metrics.SetLLMBudget(l.budget.remaining())
	return a
}

// budget is a rolling hourly token allowance, in memory and per process.
//
// In memory because spec:40 says the gateway writes two things — job rows and
// last_queried_at — and a shared budget would be a third. The consequence is
// stated rather than hidden: with n replicas the real ceiling is n times this,
// and the only control that is actually a ceiling is the provider's own
// account spend cap, which is outside this codebase.
type budget struct {
	mu    sync.Mutex
	limit int
	now   func() time.Time
	spent []spendAt
	total int
}

type spendAt struct {
	at time.Time
	n  int
}

const budgetWindow = time.Hour

func newBudget(limit int, now func() time.Time) *budget {
	if now == nil {
		now = time.Now
	}
	return &budget{limit: limit, now: now}
}

// allow reports whether a request whose WORST CASE is want tokens may start.
// The worst case, not the actual spend, because the actual spend is not known
// until the loop has already paid for it.
func (b *budget) allow(want int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	return b.total+want <= b.limit
}

// spend records what a loop actually used — reported usage where the provider
// reported it, the estimate where it did not. The Estimated flag travels in the
// trace so a reader of the gauge can tell which.
func (b *budget) spend(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	b.spent = append(b.spent, spendAt{at: b.now(), n: n})
	b.total += n
}

func (b *budget) remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	if b.total >= b.limit {
		return 0
	}
	return b.limit - b.total
}

// prune drops everything outside the window. Caller holds the lock.
func (b *budget) prune() {
	cut := b.now().Add(-budgetWindow)
	i := 0
	for ; i < len(b.spent) && !b.spent[i].at.After(cut); i++ {
		b.total -= b.spent[i].n
	}
	b.spent = b.spent[i:]
}
