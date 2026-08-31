package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/apps/gateway/internal/handler"
	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
)

type fakeQueue struct{}

func (fakeQueue) Enqueue(context.Context, string, string) (jobs.Job, error) {
	return jobs.Job{}, nil
}
func (fakeQueue) Get(context.Context, string) (jobs.Job, error) { return jobs.Job{}, nil }

// deadStore is a storeHandle with a pool aimed at an address nothing answers
// on. pgxpool.New does not dial, so the queue built from it returns an error
// rather than panicking, and the assembly can be exercised without a database.
type deadStore struct{ pool *pgxpool.Pool }

func (deadStore) Ping(context.Context) error { return errors.New("postgres is down") }
func (d deadStore) Pool() *pgxpool.Pool      { return d.pool }
func (deadStore) Close()                     {}

func serve(e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// The router has to carry both the probes and the API. Mounting one without
// the other boots a binary that passes its health check and serves nothing
// anyone asked for, which no handler test can catch.
func TestNewRouterMountsHealthAndTheAPI(t *testing.T) {
	e := newRouter(func() bool { return true }, &handler.Handler{Policy: admit.NewPolicy(admit.DefaultHosts)})

	// A refusal is answered before the queue is touched, so the API proves
	// reachable here without a database behind it.
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/health", "", http.StatusOK},
		{http.MethodGet, "/ready", "", http.StatusOK},
		{http.MethodPost, "/api/repos", `{"remote":"file:///etc/passwd"}`, http.StatusBadRequest},
	} {
		rec := serve(e, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s %s: want %d, got %d", tc.method, tc.path, tc.want, rec.Code)
		}
	}

	var mounted bool
	for _, r := range e.Routes() {
		mounted = mounted || (r.Method == http.MethodGet && r.Path == "/api/jobs/:id")
	}
	if !mounted {
		t.Error("GET /api/jobs/:id is not mounted")
	}
}

// ALLOWED_HOSTS is where an operator's list becomes a policy, and the natural
// way to write a list — with a space after the comma — must not silently
// produce a host nothing matches.
func TestAllowedHostsDefaultsAndSurvivesSpaces(t *testing.T) {
	if got := allowedHosts(); !slices.Equal(got, admit.DefaultHosts) {
		t.Errorf("unset: want %v, got %v", admit.DefaultHosts, got)
	}
	t.Setenv("ALLOWED_HOSTS", "github.com, example.test")
	p := admit.NewPolicy(allowedHosts())
	for _, raw := range []string{"https://github.com/a/b", "https://example.test/a/b"} {
		if _, err := p.Check(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if _, err := p.Check("https://codeberg.org/a/b"); err == nil {
		t.Error("a configured list must replace the default, not extend it")
	}
}

// main builds the handler once, and a field it forgets is a field no handler
// test can see missing. A zero-value zerolog.Logger discards silently — the
// property that makes the handler safe without one — so a dropped Log would
// answer every 500 with a request id and write nothing, anywhere.
func TestNewHandlerWiresTheLoggerPolicyAndQueue(t *testing.T) {
	t.Setenv("ALLOWED_HOSTS", "example.test")
	var logged bytes.Buffer
	q := fakeQueue{}
	// logger.New pins production to InfoLevel; a test logger that accepts more
	// would pass on a line production never writes.
	h := newHandler(zerolog.New(&logged).Level(zerolog.InfoLevel), q)

	h.Log.Error().Msg("ping")
	if logged.Len() == 0 {
		t.Error("the handler carries no live writer: in production a 500 would be silent")
	}
	// ALLOWED_HOSTS has to reach the policy, not just be parseable.
	if _, err := h.Policy.Check("https://example.test/a/b"); err != nil {
		t.Errorf("configured host refused: %v", err)
	}
	if _, err := h.Policy.Check("https://github.com/a/b"); err == nil {
		t.Error("the default list is still in force: ALLOWED_HOSTS was ignored")
	}
	if h.Jobs != q {
		t.Errorf("queue not wired: %#v", h.Jobs)
	}
}

// newServer is everything main assembles, and each line of it can be dropped in
// silence: calling server.New instead of newRouter would serve the probes and
// no API at all, and jobs.New(nil) would panic into echo's recover rather than
// answer.
//
// main's own call is reachable as well, through openStore. Pinning it needs an
// os.Stdout swap and a self-directed SIGTERM for one argument, which was judged
// not worth the flake risk — see newServer's comment.
func TestNewServerAssemblesWhatMainServes(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/x")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var logged bytes.Buffer
	e := newServer(zerolog.New(&logged).Level(zerolog.InfoLevel), deadStore{pool})

	// Readiness has to reflect the dependency, not a constant.
	if rec := serve(e, http.MethodGet, "/health", ""); rec.Code != http.StatusOK {
		t.Errorf("/health: want 200, got %d", rec.Code)
	}
	if rec := serve(e, http.MethodGet, "/ready", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/ready: want 503 with the store down, got %d", rec.Code)
	}

	// A submission the policy accepts must reach the queue and fail there. A
	// 400 means the policy never ran, a 404 means the API is not mounted, and
	// echo's own recover body means the queue was built from a nil pool.
	rec := serve(e, http.MethodPost, "/api/repos", `{"remote":"https://github.com/a/b"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /api/repos: want 500, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error != "internal error" || out.RequestID == "" {
		t.Fatalf("not the handler's own 500: %s", rec.Body)
	}
	// And the assembled handler logs, so a 500 in production is not silent.
	if !strings.Contains(logged.String(), out.RequestID) {
		t.Errorf("no log line for request %s: %s", out.RequestID, logged.String())
	}
}
