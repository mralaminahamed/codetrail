// Package health mounts liveness, readiness, and metrics endpoints, and holds
// the readiness check both binaries answer /ready from.
package health

import (
	"context"
	"fmt"
	"io"
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
// Both binaries refuse to boot without their dependencies, so the case this
// exists for is the one boot cannot cover: losing one afterwards. Without this,
// /ready keeps answering 200 while every query behind it fails. The cache is why a
// struggling database is not probed hardest exactly when it can least answer.
//
// Here rather than copied into the indexer: a second implementation would agree
// with this one on the day it was written and then drift, and the two things
// that are easy to lose in a copy — the gauge on every check and the seeded
// true — are the two that are silent when they go.
type Readiness struct {
	deps    []Dep
	timeout time.Duration
	ttl     time.Duration
	log     zerolog.Logger
	now     func() time.Time

	mu      sync.Mutex
	checked time.Time
	ok      bool
}

// Dep is one dependency /ready covers: the name that reaches the log line, and
// the check. Named, because "not ready" with no dependency in it sends an
// operator to the wrong system.
type Dep struct {
	Name  string
	Check func(context.Context) error
}

// NewReadiness takes the Postgres ping positionally because both binaries have
// exactly one, and any further dependency by name.
func NewReadiness(log zerolog.Logger, ping func(context.Context) error, also ...Dep) *Readiness {
	// The TTL sits under the 30s probe interval an orchestrator uses, so a
	// probe never reads an answer it could have refreshed; the timeout is well
	// inside the 5s a container health check allows for the whole command.
	//
	// Seeded true, not zero: this is only reached after boot proved Postgres
	// reachable, and an unset gauge reads 0, which would alert on every start.
	metrics.SetReady(true)
	deps := append([]Dep{{Name: "postgres", Check: ping}}, also...)
	return &Readiness{deps: deps, timeout: 2 * time.Second, ttl: 10 * time.Second, log: log, now: time.Now}
}

func (r *Readiness) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.checked.IsZero() && now.Sub(r.checked) < r.ttl {
		return r.ok
	}
	was, first := r.ok, r.checked.IsZero()
	name, err := r.check()
	r.ok, r.checked = err == nil, now
	if err != nil {
		r.log.Warn().Err(err).Str("dependency", name).Msg("not ready")
	} else if !was && !first {
		r.log.Info().Msg("ready again")
	}
	metrics.SetReady(r.ok)
	return r.ok
}

// check runs the dependencies in order and stops at the first failure: one is
// enough to be not ready, and asking the rest is a round trip whose answer
// changes nothing.
//
// Each gets its own timeout rather than sharing one, so a slow dependency
// cannot make a healthy one look broken. The worst case is therefore
// len(deps) * timeout, which stays under the interval an orchestrator probes
// on.
func (r *Readiness) check() (string, error) {
	for _, d := range r.deps {
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		err := d.Check(ctx)
		cancel()
		if err != nil {
			return d.Name, err
		}
	}
	return "", nil
}

// probeTimeout bounds the container health check's one request. Well inside the
// 5s an ECS healthCheck allows for the whole command.
const probeTimeout = 3 * time.Second

// Probe makes the request a container health check would make: one GET of
// /health on this process's own listener, exiting through the caller.
//
// It exists because the gateway's image is distroless and has no shell, so a
// CMD-SHELL health check cannot work and something has to make the request. A
// second binary would be a fifth app §2 does not describe; a flag on the one
// that is already there is not.
//
// /health and never /ready: this is liveness. A container health check on
// readiness turns one shared-datastore outage into a rolling restart of every
// task, and the process that could still serve /metrics and say why is the one
// being killed.
func Probe(addr string) error {
	c := &http.Client{Timeout: probeTimeout}
	resp, err := c.Get("http://127.0.0.1" + addr + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health answered %d", resp.StatusCode)
	}
	return nil
}
