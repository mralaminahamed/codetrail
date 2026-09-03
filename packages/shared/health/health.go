// Package health mounts liveness, readiness, and metrics endpoints, and holds
// the readiness check both binaries answer /ready from.
package health

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
)

func Register(e *echo.Echo, ready func() bool) {
	e.GET("/health", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	e.GET("/ready", func(c echo.Context) error {
		if ready == nil || ready() {
			return c.NoContent(http.StatusOK)
		}
		return c.NoContent(http.StatusServiceUnavailable)
	})
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
}

// Readiness answers /ready from a live dependency check, cached for ttl.
//
// Both binaries refuse to boot without Postgres, so the case this exists for is
// the one boot cannot cover: losing it afterwards. Without this, /ready keeps
// answering 200 while every query behind it fails. The cache is why a
// struggling database is not probed hardest exactly when it can least answer.
//
// Here rather than copied into the indexer: a second implementation would agree
// with this one on the day it was written and then drift, and the two things
// that are easy to lose in a copy — the gauge on every check and the seeded
// true — are the two that are silent when they go.
type Readiness struct {
	ping    func(context.Context) error
	timeout time.Duration
	ttl     time.Duration
	log     zerolog.Logger
	now     func() time.Time

	mu      sync.Mutex
	checked time.Time
	ok      bool
}

func NewReadiness(log zerolog.Logger, ping func(context.Context) error) *Readiness {
	// The TTL sits under the 30s probe interval an orchestrator uses, so a
	// probe never reads an answer it could have refreshed; the timeout is well
	// inside the 5s a container health check allows for the whole command.
	//
	// Seeded true, not zero: this is only reached after boot proved Postgres
	// reachable, and an unset gauge reads 0, which would alert on every start.
	metrics.SetReady(true)
	return &Readiness{ping: ping, timeout: 2 * time.Second, ttl: 10 * time.Second, log: log, now: time.Now}
}

func (r *Readiness) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.checked.IsZero() && now.Sub(r.checked) < r.ttl {
		return r.ok
	}
	was, first := r.ok, r.checked.IsZero()
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	err := r.ping(ctx)
	cancel()
	r.ok, r.checked = err == nil, now
	if err != nil {
		r.log.Warn().Err(err).Str("dependency", "postgres").Msg("not ready")
	} else if !was && !first {
		r.log.Info().Msg("ready again")
	}
	metrics.SetReady(r.ok)
	return r.ok
}
