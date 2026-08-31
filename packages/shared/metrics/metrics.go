// Package metrics defines codetrail's business metrics on the default
// Prometheus registry. Labels are closed sets only: never a repo path, a file
// path, a symbol name or a question.
package metrics

import (
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
)

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
