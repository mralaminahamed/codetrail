// Package metrics defines codetrail's business metrics on the default
// Prometheus registry. Labels are closed sets only: never a repo path, a file
// path, a symbol name or a question.
package metrics

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ready mirrors what /ready last answered. `up` says the process is
	// listening, which a gateway that has lost Postgres still is.
	ready = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "codetrail_ready",
		Help: "1 when the last readiness check passed, 0 when it failed.",
	})

	retrieval = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "codetrail_retrieval_seconds",
		Help:    "Time to run the configured arms and fuse them, by retrieval mode.",
		Buckets: prometheus.DefBuckets,
	}, []string{"mode"})

	// topScore is the distribution spec §11 asks for and the instrument P6
	// reads. It is NOT the calibration: spec §9 calibrates the floor from the
	// eval's own distribution over a labelled golden set, and a histogram has
	// no label for "was this hit correct" — a confident wrong answer and a
	// confident right one land in the same bucket.
	topScore = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "codetrail_retrieval_top_score",
		Help:    "Cosine similarity of the best-ranked span. Production instrument; the floor is calibrated in P6 from the eval's labelled distribution, not from this.",
		Buckets: prometheus.LinearBuckets(0, 0.05, 21),
	})

	// answers and refusals are spec §10's two outcomes kept apart: "we had
	// nothing to say" must never be readable as "we broke", in either
	// direction.
	answers = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_answer_total",
		Help: "Answer requests by outcome: answered, refused or error.",
	}, []string{"outcome"})

	refusals = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_refusal_total",
		Help: "Refusals by reason: no_spans, below_floor or unscored.",
	}, []string{"reason"})

	// graphEdges is the phase's headline claim as a series: how much of the
	// corpus's call graph is precise. Per edge and by label, never per job —
	// a total would say nothing about the split, which is the only thing this
	// counter exists to show.
	graphEdges = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_graph_edges_total",
		Help: "Call edges written, by provenance: resolved or syntactic.",
	}, []string{"provenance"})

	// typechecks counts job outcomes, not edges. A repository whose edges are
	// all syntactic because no toolchain is installed and one whose code has
	// no in-repo calls look identical in graphEdges; this is what tells them
	// apart.
	typechecks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_typecheck_total",
		Help: "Type-check outcomes, one per job, by reason.",
	}, []string{"reason"})

	// Wider than DefBuckets' 10s ceiling: the stage runs a compiler's front
	// end over a stranger's repository and shares the job's whole budget.
	typecheckSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "codetrail_typecheck_seconds",
		Help:    "Time the type-check stage took, whatever its outcome.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
	})

	// Two gauges rather than one, so a dashboard shows a guess as a guess.
	// Spec:315 puts the number in P6; until then the value is -1 and
	// calibrated is 0, and an operator can see both without reading the code.
	scoreFloor = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "codetrail_score_floor",
		Help: "The configured cosine-similarity floor an answer must reach.",
	})

	scoreFloorCalibrated = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "codetrail_score_floor_calibrated",
		Help: "1 when the score floor was measured, 0 when it is a placeholder. It is 0 until P6 measures one.",
	})

	jobTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_job_total",
		Help: "Indexing jobs by outcome: done, retried, or failed meaning terminal.",
	}, []string{"outcome"})

	// Labelled by outcome, because "how long does a failure take" is a
	// different question from "how long does a job take": a job that fails at
	// the deadline and one refused at admission are the same counter and very
	// different histograms.
	//
	// Exponential from 1s rather than DefBuckets, whose 10s ceiling would put
	// every real job in +Inf: a job clones, walks, chunks, embeds and
	// type-checks a stranger's repository under a 600s deadline.
	jobSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "codetrail_job_seconds",
		Help:    "Time one indexing job took, by outcome.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 14),
	}, []string{"outcome"})

	// Two counters rather than one with a rule label, for the reason answers
	// and refusals are two: the rule is defined for one outcome only. It also
	// gives a rejection *rate* a denominator, which "rejections by reason"
	// alone does not.
	admissions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_admission_total",
		Help: "Repository submissions by admission outcome: accepted or rejected.",
	}, []string{"outcome"})

	admissionRejections = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_admission_rejected_total",
		Help: "Admission rejections by the rule that refused: form, scheme or host.",
	}, []string{"rule"})

	// Rows, not sweeps: a sweep that removed forty repositories and one that
	// removed none are the same event and very different facts.
	evictions = promauto.NewCounter(prometheus.CounterOpts{
		Name: "codetrail_evicted_total",
		Help: "Repository rows removed by LRU eviction.",
	})

	// A NEW COUNTER rather than a new label on codetrail_answer_total. Adding
	// an answerer label there would change every series P3's tests assert as
	// whole delta vectors and every query an operator has already written.
	answerBy = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_answer_by_total",
		Help: "Answers by what produced the prose: extractive or llm. Never what was attempted.",
	}, []string{"answerer"})

	llmStops = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_llm_stop_total",
		Help: "Bounded-loop outcomes by stop reason. final is the only one that is not a degradation.",
	}, []string{"reason"})

	// Linear, not exponential: the ceiling is single digits and the question is
	// "did it stop at 2 or at 6", which a bucket per step answers and a
	// doubling one does not.
	llmSteps = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "codetrail_llm_steps",
		Help:    "Model calls per bounded loop, whatever the outcome.",
		Buckets: prometheus.LinearBuckets(1, 1, 12),
	})

	llmTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_llm_tokens_total",
		Help: "Tokens spent by the loop, by direction. Provider-reported where the provider reports them, estimated at four characters a token where it does not.",
	}, []string{"direction"})

	// A gauge, because it is a level rather than a rate: what is left of this
	// process's hourly budget. PER PROCESS — with n replicas the real ceiling
	// is n times this.
	llmBudget = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "codetrail_llm_budget_remaining",
		Help: "Tokens left in this PROCESS's rolling hourly budget. Not shared across replicas.",
	})

	// The incremental re-index's two counters. Both live in the INDEXER, and
	// they are scraped: health.Register mounts promhttp on the indexer's probe
	// server (PROBE_PORT, default 9090).
	//
	// This comment used to say the indexer "still has no /metrics endpoint —
	// P3 recorded that gap, P4 widened it by four instruments". It was never
	// true. The commit that gave the indexer its probe surface is an ANCESTOR
	// of the commit that wrote the sentence, so this was not drift, it was
	// wrong on the day; and P4 added three instruments, not four
	// (codetrail_graph_edges_total, codetrail_typecheck_total,
	// codetrail_typecheck_seconds). Recorded that way rather than silently
	// reworded, because a count inherited from a predecessor's prose is the
	// error this project keeps finding.
	reuseSpans = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_reuse_spans_total",
		Help: "Spans by how their vector was obtained: reused from an existing row, or embedded.",
	}, []string{"outcome"})

	cloneSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codetrail_clone_skipped_total",
		Help: "Jobs by what the pre-clone commit resolution decided: skipped, cloned, or unresolved.",
	}, []string{"outcome"})

	// Version strings as labels, against this package's own rule, and the
	// exception is argued rather than assumed: a server version is bounded by
	// the database, produces one series per running deployment, and changes
	// only when somebody upgrades. It exists because compose and RDS do not
	// run the same pgvector, and the honest answer to that is to publish what
	// is actually serving rather than to claim parity.
	datastore = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "codetrail_datastore_info",
		Help: "1, labelled with the PostgreSQL and pgvector versions actually serving.",
	}, []string{"postgres", "pgvector"})
)

// The job counters' vocabulary. Constants rather than literals at the call
// sites, because the worker decides the outcome in five places.
const (
	JobDone    = "done"
	JobRetried = "retried"
	JobFailed  = "failed"
)

// GraphProvenances and TypecheckReasons are the graph counters' whole
// vocabularies, exported so a test can pin them against models.Provenances and
// symbols' reason constants. Written out rather than imported: symbols pulls in
// go/packages, and the gateway links this package.
var (
	GraphProvenances = []string{"resolved", "syntactic"}
	TypecheckReasons = []string{"ok", "disabled", "no_toolchain", "no_module", "load_error", "deadline", "policy"}

	// JobOutcomes and AdmissionRules are the two new counters' whole
	// vocabularies. AdmissionRules is admit.Rule's, written out rather than
	// imported so a test can pin the two together: importing it would make
	// that test compare admit against itself.
	JobOutcomes    = []string{JobDone, JobRetried, JobFailed}
	AdmissionRules = []string{"form", "scheme", "host"}

	// Answerers is the closed set answered_by may take, and LLMStopReasons the
	// closed set of loop outcomes. Written out rather than imported from
	// packages/shared/agent, because agent imports rag and rag imports this
	// package — the same reason AdmissionRules is written out rather than
	// imported from admit. A handler test pins these against agent.Stops, so
	// the two lists cannot drift without something failing.
	Answerers = []string{"extractive", "llm"}

	LLMStopReasons = []string{
		"final", "step_limit", "tool_call_limit", "token_budget", "deadline",
		"malformed_tool_call", "tool_error", "repeated_tool_call", "uncited",
		"busy", "budget_exhausted",
		"rate_limited", "unauthorized", "provider_unavailable", "malformed_response",
	}

	// TokenDirections is llmTokens' whole vocabulary.
	TokenDirections = []string{"input", "output"}

	// The re-index vocabularies. unresolved is not a failure: it means the ref
	// did not resolve to exactly one head and the job cloned, which is the
	// fall-through the fast path is designed around.
	ReuseOutcomes = []string{"reused", "embedded"}
	CloneOutcomes = []string{"skipped", "cloned", "unresolved"}
)

// The label sets are closed, and these are their whole vocabularies — rag's
// Mode, Outcome and Reason constants, plus the "error" outcome only a handler
// can know about. Written out here rather than imported because rag calls into
// this package.
//
// Initialised to zero at startup so a counter that has not moved reads as 0
// rather than as an absent series: rate() over an absent series is nothing at
// all, and an alert on "refusals over answers" needs both to exist before the
// first request.
func init() {
	for _, mode := range []string{"vector", "lexical", "hybrid"} {
		retrieval.WithLabelValues(mode)
	}
	for _, outcome := range []string{"answered", "refused", "error"} {
		answers.WithLabelValues(outcome)
	}
	for _, reason := range []string{"no_spans", "below_floor", "unscored"} {
		refusals.WithLabelValues(reason)
	}
	for _, p := range GraphProvenances {
		graphEdges.WithLabelValues(p)
	}
	for _, reason := range TypecheckReasons {
		typechecks.WithLabelValues(reason)
	}
	for _, outcome := range JobOutcomes {
		jobTotal.WithLabelValues(outcome)
		jobSeconds.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"accepted", "rejected"} {
		admissions.WithLabelValues(outcome)
	}
	for _, rule := range AdmissionRules {
		admissionRejections.WithLabelValues(rule)
	}
	for _, a := range Answerers {
		answerBy.WithLabelValues(a)
	}
	for _, r := range LLMStopReasons {
		llmStops.WithLabelValues(r)
	}
	for _, d := range TokenDirections {
		llmTokens.WithLabelValues(d)
	}
	for _, o := range ReuseOutcomes {
		reuseSpans.WithLabelValues(o)
	}
	for _, o := range CloneOutcomes {
		cloneSkipped.WithLabelValues(o)
	}
	// datastore is deliberately not seeded: its labels are not known until a
	// connection exists, and a series labelled with an empty version would
	// claim a fact nobody has read yet.
}

// ObserveRetrieval records how long the arms and the fusion took. The
// retriever calls it: it is the only thing that knows what "retrieval latency"
// covers, which is why the outcome counters below are somebody else's job.
func ObserveRetrieval(mode string, d time.Duration) {
	retrieval.WithLabelValues(mode).Observe(d.Seconds())
}

// ObserveTopScore records the vector arm's best cosine similarity.
//
// Only a real number: a single NaN observation makes the histogram's _sum NaN
// for the lifetime of the process, and no dashboard recovers from that. A
// lexical-only run and an empty result have no such score, and pgvector
// answers NaN for a zero vector's distance.
func ObserveTopScore(v float64) {
	if math.IsNaN(v) {
		return
	}
	topScore.Observe(v)
}

// CountAnswer counts one answer request by its outcome, and CountRefusal the
// reason behind a refusal. Two counters, because the reason is only defined
// for one of the three outcomes and a label that is empty two thirds of the
// time is a label nobody can group by.
func CountAnswer(outcome string) { answers.WithLabelValues(outcome).Inc() }

func CountRefusal(reason string) { refusals.WithLabelValues(reason).Inc() }

// CountGraph counts one written edge by its provenance, CountTypecheck one
// job's type-check by why it stopped, and ObserveTypecheck how long that took.
//
// Three instruments because they answer three questions an operator asks
// separately: how precise is the corpus, why is it not more precise, and what
// does the precision cost.
func CountGraph(provenance string) { graphEdges.WithLabelValues(provenance).Inc() }

func CountTypecheck(reason string) { typechecks.WithLabelValues(reason).Inc() }

func ObserveTypecheck(d time.Duration) { typecheckSeconds.Observe(d.Seconds()) }

// SetFloor publishes the floor an operator is running with and whether anyone
// measured it. Called at boot, so the pair is on the dashboard before the
// first query rather than after it.
func SetFloor(value float64, calibrated bool) {
	scoreFloor.Set(value)
	if calibrated {
		scoreFloorCalibrated.Set(1)
		return
	}
	scoreFloorCalibrated.Set(0)
}

// CountJob records one job's outcome and how long it took, once per job, from
// the worker that ran it. The duration goes in under the same outcome as the
// count, so the two instruments cannot disagree about what happened.
func CountJob(outcome string, d time.Duration) {
	jobTotal.WithLabelValues(outcome).Inc()
	jobSeconds.WithLabelValues(outcome).Observe(d.Seconds())
}

// CountAdmission records one admission decision and, for a refusal, the rule
// behind it. rule is read only when accepted is false.
//
// Called by the gateway only. The indexer re-checks the same allowlist before
// it clones, and counting there too would double every submission; that
// re-check failing is a job failure with a reason and belongs to CountJob.
func CountAdmission(accepted bool, rule string) {
	if accepted {
		admissions.WithLabelValues("accepted").Inc()
		return
	}
	admissions.WithLabelValues("rejected").Inc()
	admissionRejections.WithLabelValues(rule).Inc()
}

// CountEvicted counts the rows one sweep removed.
func CountEvicted(n int) { evictions.Add(float64(n)) }

// SetDatastoreInfo publishes the versions the process is actually talking to,
// read from the server rather than from the image tag that was asked for.
func SetDatastoreInfo(postgres, pgvector string) {
	datastore.WithLabelValues(postgres, pgvector).Set(1)
}

// CountAnswerBy records what wrote the prose the caller is reading — never
// what was attempted. A degraded request counts as extractive here and as
// answered on codetrail_answer_total, because the caller got an answer.
func CountAnswerBy(answerer string) { answerBy.WithLabelValues(answerer).Inc() }

// CountLLMStop records why one bounded loop ended, once per loop that ran.
func CountLLMStop(reason string) { llmStops.WithLabelValues(reason).Inc() }

// ObserveLLM records one loop's work: how many model calls it made and what it
// spent. Called for every loop that made at least one model call, a degraded
// one included — a degradation that cost four steps and 9,000 tokens is spend
// an operator has to see.
//
// Not for a loop that made none. The buckets below start at 1, so a 0 lands in
// le="1" and is indistinguishable from a single call; the caller's stop counter
// is what records those.
func ObserveLLM(steps, inputTokens, outputTokens int) {
	llmSteps.Observe(float64(steps))
	llmTokens.WithLabelValues("input").Add(float64(inputTokens))
	llmTokens.WithLabelValues("output").Add(float64(outputTokens))
}

// SetLLMBudget publishes what is left of this process's hourly token budget.
func SetLLMBudget(remaining int) { llmBudget.Set(float64(remaining)) }

// CountReuse records how n spans got their vectors. Called once per outcome
// per job, so "reused" and "embedded" always sum to the job's span count.
func CountReuse(outcome string, n int) {
	if n > 0 {
		reuseSpans.WithLabelValues(outcome).Add(float64(n))
	}
}

// CountCloneSkipped records what the pre-clone resolution decided for one job.
func CountCloneSkipped(outcome string) { cloneSkipped.WithLabelValues(outcome).Inc() }

// SetReady records the outcome of a readiness check. A service that never
// calls it leaves the gauge at zero, which reads as not-ready — so callers set
// it at construction as well as on every check.
func SetReady(ok bool) {
	if ok {
		ready.Set(1)
		return
	}
	ready.Set(0)
}
