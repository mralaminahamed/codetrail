package health

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
)

func TestHealthAlwaysOK(t *testing.T) {
	e := echo.New()
	Register(e, func() bool { return false })
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health want 200, got %d", rec.Code)
	}
}

func TestReadyReflectsProbe(t *testing.T) {
	e := echo.New()
	ready := false
	Register(e, func() bool { return ready })
	do := func() int {
		req := httptest.NewRequest(http.MethodGet, "/ready", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	if do() != http.StatusServiceUnavailable {
		t.Fatal("ready should be 503 when not ready")
	}
	ready = true
	if do() != http.StatusOK {
		t.Fatal("ready should be 200 when ready")
	}
}

// readyGauge reads codetrail_ready out of the default registry, which is what
// an alert on "up but not ready" actually selects on.
func readyGauge(t *testing.T) float64 {
	t.Helper()
	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "codetrail_ready "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("codetrail_ready is %q: %v", v, err)
			}
			return f
		}
	}
	t.Fatal("codetrail_ready is not exported at all")
	return 0
}

// probe counts the checks a Readiness actually made, so "cached" can be
// asserted as "the dependency was not asked" rather than as "the answer was
// the same".
type probe struct {
	calls int
	err   error
}

func (p *probe) ping(context.Context) error { p.calls++; return p.err }

// withClock is what a test needs of the internals: the TTL is a duration, and
// waiting it out in real time would make this suite ten seconds long.
func withClock(r *Readiness, now func() time.Time) *Readiness {
	r.now = now
	return r
}

func TestReadinessCachesWithinItsTTLAndRefreshesAfterIt(t *testing.T) {
	p := &probe{}
	at := time.Unix(0, 0)
	r := withClock(NewReadiness(zerolog.Nop(), p.ping), func() time.Time { return at })

	r.Ready()
	r.Ready()
	if p.calls != 1 {
		t.Fatalf("the dependency was probed %d times inside the TTL, want 1", p.calls)
	}
	at = at.Add(r.ttl)
	r.Ready()
	if p.calls != 2 {
		t.Fatalf("the dependency was probed %d times across the TTL, want 2", p.calls)
	}
}

func TestReadinessSetsTheGaugeOnEveryCheck(t *testing.T) {
	p := &probe{}
	at := time.Unix(0, 0)
	r := withClock(NewReadiness(zerolog.Nop(), p.ping), func() time.Time { return at })

	if r.Ready() != true || readyGauge(t) != 1 {
		t.Fatal("a reachable dependency must leave the gauge at 1")
	}
	p.err = errors.New("postgres is gone")
	at = at.Add(r.ttl)
	if r.Ready() != false || readyGauge(t) != 0 {
		t.Fatalf("a lost dependency must set the gauge to 0, got %v", readyGauge(t))
	}
	// And back: a gauge that only ever falls is one an operator learns to
	// ignore.
	p.err = nil
	at = at.Add(r.ttl)
	if r.Ready() != true || readyGauge(t) != 1 {
		t.Fatal("a recovered dependency must set the gauge back to 1")
	}
}

// A fresh process has never run a check, so the gauge would read 0 — which is
// exactly what NotReady alerts on — for as long as it took the first probe to
// arrive.
func TestReadinessSeedsTrueSoAFreshProcessDoesNotAlert(t *testing.T) {
	metrics.SetReady(false)
	p := &probe{}
	NewReadiness(zerolog.Nop(), p.ping)
	if readyGauge(t) != 1 {
		t.Fatalf("codetrail_ready is %v at construction, want 1", readyGauge(t))
	}
	if p.calls != 0 {
		t.Fatalf("construction probed the dependency %d times; the seed is a seed, not a check", p.calls)
	}
}
