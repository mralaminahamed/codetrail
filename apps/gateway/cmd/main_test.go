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
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
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

// The retrieval arms, which nothing in this file calls: the retriever is
// constructed at boot and served from in the next task. They return the error a
// dead store would.
func (deadStore) VectorSearch(context.Context, string, []float32, int) ([]models.Cite, error) {
	return nil, errors.New("postgres is down")
}
func (deadStore) LexicalSearch(context.Context, string, []string, int) ([]models.Cite, error) {
	return nil, errors.New("postgres is down")
}
func (deadStore) SpanEmbedder(context.Context, string) (string, int, error) {
	return "", 0, errors.New("postgres is down")
}

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

// bootEnv is the configuration every boot test starts from: the fake embedder,
// so no test depends on an Ollama being up on the machine it runs on. The
// default provider is ollama, and a developer with one running would otherwise
// get a different result from CI.
func bootEnv(t *testing.T) {
	t.Helper()
	t.Setenv("EMBED_PROVIDER", "fake")
}

func boot(t *testing.T) (*rag.Retriever, *bytes.Buffer, error) {
	t.Helper()
	var logged bytes.Buffer
	r, err := newRetriever(context.Background(), zerolog.New(&logged).Level(zerolog.InfoLevel), deadStore{})
	return r, &logged, err
}

// An embedder of the wrong width means every query is ranked against a column
// it cannot be compared to. The indexer refuses to boot for this; a gateway
// that did not would answer confident nonsense on every query instead.
func TestGatewayRefusesToBootOnAnEmbedderOfTheWrongWidth(t *testing.T) {
	bootEnv(t)
	t.Setenv("EMBED_DIM", "64")
	if _, _, err := boot(t); !errors.Is(err, store.ErrDimMismatch) {
		t.Fatalf("want ErrDimMismatch, got %v", err)
	}
}

// Parity with the indexer, and the cost of it: with EMBED_PROVIDER=ollama and
// nothing listening, the gateway does not start at all. The alternative — boot
// into a mode where retrieval 503s and submission works — is a silent
// downgrade, and there is no state in this codebase for one.
func TestGatewayRefusesToBootWhenTheEmbedderIsUnreachable(t *testing.T) {
	// A listener that is closed: well-formed address, nothing there.
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := gone.URL
	gone.Close()

	t.Setenv("EMBED_PROVIDER", "ollama")
	t.Setenv("OLLAMA_URL", url)
	_, _, err := boot(t)
	if err == nil {
		t.Fatal("the gateway booted with no embedder behind it")
	}
	if !strings.Contains(err.Error(), "EMBED_MODEL=") {
		t.Fatalf("the error does not name the knob to look at: %v", err)
	}
}

// A typo in RETRIEVAL_MODE must not fall back to a mode the operator did not
// ask for: the mode decides which arms run, and a service retrieving
// differently than it was configured to is unreadable from the outside.
func TestGatewayRefusesToBootOnAnUnknownRetrievalMode(t *testing.T) {
	bootEnv(t)
	t.Setenv("RETRIEVAL_MODE", "hybrid ")
	if _, _, err := boot(t); err == nil {
		t.Fatal("a mode with a trailing space was accepted")
	}
	t.Setenv("RETRIEVAL_MODE", "Hybrid")
	if _, _, err := boot(t); err == nil {
		t.Fatal("a mode of the wrong case was accepted")
	}
}

// Fail closed like chunk.Options and walk.Limits. A floor outside the cosine
// range is a misconfiguration, not a preference: 7 refuses every answer there
// can ever be, and 0.5 spelt "0,5" would silently revert to the default and
// leave an operator believing a filter is running.
func TestGatewayRefusesAFloorOutsideTheCosineRange(t *testing.T) {
	for _, v := range []string{"7", "-1.5", "NaN", "0,5", "half"} {
		t.Run(v, func(t *testing.T) {
			bootEnv(t)
			t.Setenv("ANSWER_SCORE_FLOOR", v)
			if _, _, err := boot(t); err == nil {
				t.Fatalf("ANSWER_SCORE_FLOOR=%s was accepted", v)
			}
		})
	}
}

// Fuse divides by k+rank and has no guard of its own — Task 1 left this here on
// purpose. k=-1 makes the top hit's contribution +Inf and k<=-2 inverts the
// ranking, and neither says anything at any point.
func TestGatewayRefusesAFusionConstantFuseCannotSurvive(t *testing.T) {
	bootEnv(t)
	for _, v := range []string{"-1", "-2"} {
		t.Setenv("RETRIEVAL_RRF_K", v)
		if _, _, err := boot(t); err == nil {
			t.Fatalf("RETRIEVAL_RRF_K=%s was accepted", v)
		}
	}
	t.Setenv("RETRIEVAL_RRF_K", "0")
	if _, _, err := boot(t); err != nil {
		t.Fatalf("k=0 is reciprocal rank with no discount, not a misconfiguration: %v", err)
	}
	t.Setenv("RETRIEVAL_RRF_K", "60")
	t.Setenv("RETRIEVAL_CANDIDATES", "0")
	if _, _, err := boot(t); err == nil {
		t.Fatal("a candidate depth of 0 was accepted; both arms would return nothing")
	}
}

// The gauges are how an operator sees a guess as a guess, and they are read
// from the same value the retriever runs with rather than set from a literal
// somewhere else.
func TestTheFloorGaugeIsSetFromTheConfiguredValue(t *testing.T) {
	bootEnv(t)
	t.Setenv("ANSWER_SCORE_FLOOR", "0.25")
	r, _, err := boot(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.Floor.Value != 0.25 || r.Floor.Calibrated {
		t.Fatalf("retriever floor %+v, want 0.25 uncalibrated", r.Floor)
	}
	body := serve(newRouter(func() bool { return true }, &handler.Handler{}), http.MethodGet, "/metrics", "").Body.String()
	for _, want := range []string{
		"codetrail_score_floor 0.25",
		// An operator's own number is still not one this project measured.
		"codetrail_score_floor_calibrated 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not carry %q", want)
		}
	}
}

// The shipped defaults, all of them, in one place: hybrid because spec §8
// defines retrieval as hybrid, k=60 from the paper, 40 candidates because that
// is pgvector's hnsw.ef_search default, and a floor of -1 that cannot exclude
// anything. If any of these moves, the README moved with it.
func TestTheDefaultRetrieverIsHybridAtTheUncalibratedFloor(t *testing.T) {
	bootEnv(t)
	r, logged, err := boot(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != rag.ModeHybrid || r.K != 60 || r.Candidates != 40 || !r.Split {
		t.Errorf("mode=%s k=%d candidates=%d split=%v", r.Mode, r.K, r.Candidates, r.Split)
	}
	if r.Floor != rag.DefaultFloor() || r.Floor.Value != -1 || r.Floor.Calibrated {
		t.Errorf("floor %+v, want -1 uncalibrated", r.Floor)
	}
	if r.Store == nil || r.Emb == nil {
		t.Fatalf("retriever built without a store or an embedder: %+v", r)
	}
	// The boot log is one of the four places the floor has to say it is a
	// placeholder (the payload, the gauges, this line, the README).
	line := logged.String()
	if !strings.Contains(line, `"score_floor":-1`) || !strings.Contains(line, `"floor_calibrated":false`) {
		t.Errorf("the boot log does not carry the floor and its calibration: %s", line)
	}
	if !strings.Contains(line, "not calibrated") {
		t.Errorf("the boot log does not say the floor is uncalibrated: %s", line)
	}
}

// LEXICAL_SPLIT_IDENTIFIERS is what P6 sweeps to find out whether query-side
// splitting helps, so it has to reach the retriever, and a value that is not a
// boolean must not read as false.
func TestSplittingIdentifiersIsAKnobThatFailsClosed(t *testing.T) {
	bootEnv(t)
	t.Setenv("LEXICAL_SPLIT_IDENTIFIERS", "false")
	r, _, err := boot(t)
	if err != nil || r.Split {
		t.Fatalf("split=%v err=%v, want it off", r != nil && r.Split, err)
	}
	t.Setenv("LEXICAL_SPLIT_IDENTIFIERS", "yes please")
	if _, _, err := boot(t); err == nil {
		t.Fatal("a non-boolean was accepted")
	}
}
