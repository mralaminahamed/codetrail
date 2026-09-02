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
)

// GraphProvenances and TypecheckReasons are the graph counters' whole
// vocabularies, exported so a test can pin them against models.Provenances and
// symbols' reason constants. Written out rather than imported: symbols pulls in
// go/packages, and the gateway links this package.
var (
	GraphProvenances = []string{"resolved", "syntactic"}
	TypecheckReasons = []string{"ok", "disabled", "no_toolchain", "no_module", "load_error", "deadline", "policy"}
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
