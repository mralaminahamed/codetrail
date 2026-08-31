// Command gateway serves codetrail's HTTP API.
package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/apps/gateway/internal/handler"
	"github.com/mralaminahamed/codetrail/apps/gateway/internal/server"
	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/logger"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
)

// pinger is what readinessFor needs of the store. An interface so the
// composition can be tested without a Postgres to connect to.
type pinger interface{ Ping(context.Context) error }

// readiness answers /ready from a live dependency check, cached for ttl.
//
// The gateway refuses to boot without Postgres, so the case this exists for is
// the one boot cannot cover: losing it afterwards. Without this, /ready keeps
// answering 200 while every query behind it fails. The cache is why a
// struggling database is not probed hardest exactly when it can least answer.
type readiness struct {
	ping    func(context.Context) error
	timeout time.Duration
	ttl     time.Duration
	log     zerolog.Logger
	now     func() time.Time

	mu      sync.Mutex
	checked time.Time
	ok      bool
}

func readinessFor(log zerolog.Logger, st pinger) *readiness {
	// The TTL sits under the 30s probe interval an orchestrator uses, so a
	// probe never reads an answer it could have refreshed; the timeout is well
	// inside the 5s a container health check allows for the whole command.
	//
	// Seeded true, not zero: this is only reached after boot proved Postgres
	// reachable, and an unset gauge reads 0, which would alert on every start.
	metrics.SetReady(true)
	return &readiness{ping: st.Ping, timeout: 2 * time.Second, ttl: 10 * time.Second, log: log, now: time.Now}
}

func (r *readiness) Ready() bool {
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

// newRouter builds the router the binary actually serves. A function rather
// than inline in main so a test can pin the composition.
func newRouter(ready func() bool, h *handler.Handler) *echo.Echo {
	e := server.New(ready)
	handler.Mount(e, h)
	return e
}

// allowedHosts reads the exact-host allowlist: ALLOWED_HOSTS, comma-separated,
// replacing the default rather than extending it. NewPolicy trims, so a list
// written with spaces works.
//
// A host whose repository paths nest deeper than /owner/name is only half
// served by adding it — gitlab.com's group/repo would be accepted and its
// group/subgroup/repo refused — because Check requires exactly two segments.
func allowedHosts() []string {
	return strings.Split(config.Get("ALLOWED_HOSTS", strings.Join(admit.DefaultHosts, ",")), ",")
}

func main() {
	log := logger.New("gateway")
	ctx := context.Background()

	dsn := config.Get("DATABASE_URL", "postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable")
	st, err := openStore(ctx, dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("connect postgres")
	}
	defer st.Close()
	log.Info().Msg("postgres ready, schema up to date")

	h := &handler.Handler{Policy: admit.NewPolicy(allowedHosts()), Jobs: jobs.New(st.Pool())}
	e := newRouter(readinessFor(log, st).Ready, h)

	addr := ":" + config.Get("PORT", "8080")
	go func() {
		if err := e.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("server exited")
		}
	}()
	log.Info().Str("addr", addr).Msg("gateway up")

	sctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-sctx.Done()

	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = e.Shutdown(shCtx)
}
