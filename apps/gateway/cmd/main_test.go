package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/apps/gateway/internal/handler"
	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/agent"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
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

// The retrieval arms and the read path's queries. All of them answer what a
// dead store answers, so the assembly can be driven without a database and a
// route that is wired reaches its 500 rather than a 404.
func (deadStore) VectorSearch(context.Context, string, []float32, int) ([]models.Cite, error) {
	return nil, errors.New("postgres is down")
}
func (deadStore) LexicalSearch(context.Context, string, []string, int) ([]models.Cite, error) {
	return nil, errors.New("postgres is down")
}
func (deadStore) SpanEmbedder(context.Context, string) (string, int, error) {
	return "", 0, errors.New("postgres is down")
}
func (deadStore) ListRepos(context.Context, int) ([]store.RepoRow, error) {
	return nil, errors.New("postgres is down")
}
func (deadStore) GetRepo(context.Context, string) (models.Repo, error) {
	return models.Repo{}, errors.New("postgres is down")
}
func (deadStore) RepoGone(context.Context, string) (bool, error) {
	return false, errors.New("postgres is down")
}
func (deadStore) RepoStats(context.Context, string) (store.Stats, error) {
	return store.Stats{}, errors.New("postgres is down")
}
func (deadStore) GetSpan(context.Context, string, string) (models.Span, error) {
	return models.Span{}, errors.New("postgres is down")
}
func (deadStore) NewerCommit(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, errors.New("postgres is down")
}
func (deadStore) TouchRepo(context.Context, string) error { return errors.New("postgres is down") }
func (deadStore) Definitions(context.Context, string, string, string, bool, int) ([]models.Symbol, error) {
	return nil, errors.New("postgres is down")
}
func (deadStore) Symbol(context.Context, string, string) (models.Symbol, error) {
	return models.Symbol{}, errors.New("postgres is down")
}
func (deadStore) CallersOf(context.Context, string, string, int, int) ([]store.Caller, error) {
	return nil, errors.New("postgres is down")
}
func (deadStore) ApproximateCallersOf(context.Context, string, string, int) ([]store.Approximate, error) {
	return nil, errors.New("postgres is down")
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
	st := deadStore{}
	ret := &rag.Retriever{Store: st, Mode: rag.ModeHybrid, Fusion: rag.DefaultParams(), Candidates: 40,
		Floor: rag.Floor{Value: 0.25, Calibrated: false}}
	budget := rag.Budget{MaxSpans: 3, MaxChars: 4096}
	h := newHandler(zerolog.New(&logged).Level(zerolog.InfoLevel), q, st, ret, budget, nil, "extractive")

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
	if h.Repos != handler.Reader(st) {
		t.Errorf("read store not wired: %#v", h.Repos)
	}
	if h.Rag != ret {
		t.Errorf("retriever not wired: %#v", h.Rag)
	}
	// Values, not presence: the floor in the payload has to be the floor the
	// decision used, and a budget of zero would answer every question with an
	// empty string.
	if h.Floor != ret.Floor {
		t.Errorf("floor %+v, want the retriever's %+v", h.Floor, ret.Floor)
	}
	if h.Budget != budget {
		t.Errorf("budget %+v, want %+v", h.Budget, budget)
	}
	if h.Now == nil {
		t.Error("no clock: every citation would be dated from the zero time")
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
	e := newServer(zerolog.New(&logged).Level(zerolog.InfoLevel), deadStore{pool},
		&rag.Retriever{Store: deadStore{pool}, Emb: embed.NewFake(store.EmbeddingDim), Mode: rag.ModeHybrid,
			Fusion: rag.DefaultParams(), Candidates: 40, Floor: rag.DefaultFloor()},
		rag.DefaultBudget(), nil, "extractive")

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

	// The read routes are part of the assembly too, and a handler built without
	// a store would 404 them rather than fail on the dead one.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/repos", ""},
		{http.MethodGet, "/api/repos/r1", ""},
		{http.MethodGet, "/api/repos/r1/spans/s1", ""},
		{http.MethodPost, "/api/repos/r1/search", `{"q":"parse"}`},
		{http.MethodPost, "/api/repos/r1/ask", `{"q":"parse"}`},
	} {
		if rec := serve(e, tc.method, tc.path, tc.body); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s: want 500 from the dead store, got %d: %s", tc.method, tc.path, rec.Code, rec.Body)
		}
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

// The shipped defaults, all of them, in one place: VECTOR since P7, on a
// measurement rather than on conformance; k=60 from the paper; 40 candidates
// because that is pgvector's hnsw.ef_search default; unit fusion weights; and a
// floor of -1 that cannot exclude anything. If any of these moves, the README
// moved with it — which the test below asserts rather than asks.
func TestTheDefaultRetrieverIsVectorAtTheUncalibratedFloor(t *testing.T) {
	bootEnv(t)
	r, logged, err := boot(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != rag.ModeVector || r.Fusion != rag.DefaultParams() || r.Candidates != 40 || !r.Split {
		t.Errorf("mode=%s fusion=%+v candidates=%d split=%v", r.Mode, r.Fusion, r.Candidates, r.Split)
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

// Every integer knob the gateway reads, with a letter O for a zero. Before
// this, all five booted on their defaults and logged the default back: a
// service retrieving 40 candidates while its operator wrote 4O is the same
// wrong-but-quiet class as a floor that reverted to -1, and the range checks
// beside them never saw the value.
//
// The bad value has to be one Atoi rejects but a human reads as the number, so
// the test covers the mistake that actually happens rather than "abc".
func TestGatewayRefusesAnIntegerKnobThatIsNotAnInteger(t *testing.T) {
	for _, key := range []string{
		"RETRIEVAL_RRF_K", "RETRIEVAL_CANDIDATES", "EMBED_DIM",
	} {
		t.Run(key, func(t *testing.T) {
			bootEnv(t)
			t.Setenv(key, "4O")
			_, _, err := boot(t)
			if err == nil {
				t.Fatalf("%s=4O booted", key)
			}
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "4O") {
				t.Errorf("the error names neither the setting nor the value: %v", err)
			}
		})
	}
	for _, key := range []string{"ANSWER_MAX_SPANS", "ANSWER_MAX_CHARS"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "4O")
			_, err := answerBudget()
			if err == nil {
				t.Fatalf("%s=4O booted", key)
			}
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "4O") {
				t.Errorf("the error names neither the setting nor the value: %v", err)
			}
		})
	}
	// And the defaults still boot: a refusal that fired on an unset knob would
	// pass every assertion above and start nothing.
	if b, err := answerBudget(); err != nil || b != rag.DefaultBudget() {
		t.Fatalf("the defaults must boot: %+v, %v", b, err)
	}
}

// The flag says *codetrail* measured this number. An operator's own value is
// not one this project has evidence for, whatever the shipped default becomes.
//
// The environment is set on purpose: with none the default is correct by
// construction, so a no-environment test — which is what this suite had —
// cannot discriminate. Today DefaultFloor().Calibrated is false and this passes
// either way; the day a floor is measured, it is the only thing between a
// hand-typed number and a gauge claiming it was measured. Observed under a
// calibrated default of {0.62, true} with the old spelling: floor is
// {Value:0.5 Calibrated:true}, want {Value:0.5 Calibrated:false}.
func TestAnOperatorsFloorIsNeverLabelledCalibrated(t *testing.T) {
	t.Setenv("ANSWER_SCORE_FLOOR", "0.5")
	got, err := scoreFloor()
	if err != nil {
		t.Fatal(err)
	}
	if want := (rag.Floor{Value: 0.5, Calibrated: false}); got != want {
		t.Errorf("floor is %+v, want %+v", got, want)
	}
}

// And codetrail's own number keeps whatever label DefaultFloor gives it, so
// the two cannot be confused.
func TestTheUnsetFloorIsTheShippedDefaultWhole(t *testing.T) {
	if got, err := scoreFloor(); err != nil || got != rag.DefaultFloor() {
		t.Errorf("with no environment the floor is %+v (%v), want %+v", got, err, rag.DefaultFloor())
	}
}

// A default that moved with no published measurement beside it is the "guess
// wearing a measurement's clothes" the -1 floor was chosen to prevent, and the
// README is where a reader checks. A test on the constant alone cannot detect a
// README that still says hybrid.
func TestTheDefaultRetrievalModeMatchesWhatTheReadmeRecords(t *testing.T) {
	bootEnv(t)
	r, _, err := boot(t)
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(filepath.Join("..", "..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	sec := section(t, string(readme), "## Retrieval")
	want := "`RETRIEVAL_MODE` defaults to `" + string(r.Mode) + "`"
	if !strings.Contains(sec, want) {
		t.Errorf("boot default %q; the README's Retrieval section does not say %q", r.Mode, want)
	}
	// And the numbers that moved it, so the default and its evidence cannot
	// drift apart. Each of these is a figure from the run, not a rounding
	// anybody may adjust.
	for _, n := range []string{"0.7492", "0.4023", "0.1637", "google/uuid", "2d3c2a9", "74"} {
		if !strings.Contains(sec, n) {
			t.Errorf("the README's Retrieval section does not carry %q", n)
		}
	}
	// The SCOPED CLAIM itself, verbatim, not a phrase that could appear
	// incidentally. Measured: asserting only that the words "one corpus" occur
	// somewhere in the section passed under a mutation that deleted every
	// hedge, because a different sentence about P6's floor also contains them.
	// A result stated without its scope is a broader claim than the run
	// supports, and this is the sentence that carries the scope.
	for _, claim := range []string{
		"fusion does not help on doc-comment prose queries, on this corpus, at these settings",
		"Measured on **one corpus**",
		"no identifier-lookup question at all",
	} {
		if !strings.Contains(sec, claim) {
			t.Errorf("the README's Retrieval section does not state the limit %q", claim)
		}
	}
}

// section returns one "## " heading's body.
func section(t *testing.T, doc, heading string) string {
	t.Helper()
	i := strings.Index(doc, heading)
	if i < 0 {
		t.Fatalf("the README has no %q section", heading)
	}
	rest := doc[i+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// ---- the answering loop's boot contract -----------------------------------

func llmEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY", "LLM_API_KEY_FILE",
		"LLM_BELOW_FLOOR", "LLM_MAX_CONCURRENT", "LLM_TOKENS_PER_HOUR", "ANSWER_DEFAULT",
	} {
		t.Setenv(k, "")
	}
}

func answeringBoot(t *testing.T) (handler.Answerer, string, *bytes.Buffer, error) {
	t.Helper()
	var logged bytes.Buffer
	loop, def, err := answering(zerolog.New(&logged).Level(zerolog.InfoLevel), deadStore{}, &rag.Retriever{})
	return loop, def, &logged, err
}

// spec:230-232's default. With no provider the loop does not exist: no client,
// no key, and nothing that could make an offline deploy look degraded.
func TestTheDefaultDeploymentHasNoLoopAndIsNotDegraded(t *testing.T) {
	llmEnv(t)
	loop, def, logged, err := answeringBoot(t)
	if err != nil {
		t.Fatalf("the default configuration does not boot: %v", err)
	}
	if loop != nil {
		t.Errorf("a loop was built with no provider configured: %v", loop)
	}
	if def != "extractive" {
		t.Errorf("ANSWER_DEFAULT defaulted to %q, want extractive", def)
	}
	if !strings.Contains(logged.String(), "nothing is a degradation") {
		t.Errorf("the boot log does not say an unconfigured deployment is not a degraded one: %s", logged)
	}
}

// A setting an operator believes is in force and is not. Every other knob in
// this codebase refuses at boot for this reason.
func TestAnswerDefaultLlmWithoutAProviderRefusesToBoot(t *testing.T) {
	llmEnv(t)
	t.Setenv("ANSWER_DEFAULT", "llm")
	_, _, _, err := answeringBoot(t)
	if err == nil {
		t.Fatalf("booted with ANSWER_DEFAULT=llm and LLM_PROVIDER=none")
	}
	for _, want := range []string{"ANSWER_DEFAULT", "LLM_PROVIDER"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the boot error does not name %s: %v", want, err)
		}
	}
}

func TestAnUnknownAnswerDefaultRefusesToBoot(t *testing.T) {
	llmEnv(t)
	t.Setenv("ANSWER_DEFAULT", "extractve")
	if _, _, _, err := answeringBoot(t); err == nil {
		t.Errorf("booted with ANSWER_DEFAULT=extractve")
	}
}

// The house rule: parsed, never compared against "true". TRUE and 1 are
// booleans ParseBool accepts and == "true" does not.
func TestTheLoopsBooleanKnobIsParsedNotCompared(t *testing.T) {
	for _, v := range []string{"TRUE", "1", "t", "True"} {
		llmEnv(t)
		t.Setenv("LLM_PROVIDER", "fake")
		t.Setenv("LLM_BELOW_FLOOR", v)
		loop, _, _, err := answeringBoot(t)
		if err != nil {
			t.Fatalf("LLM_BELOW_FLOOR=%s: %v", v, err)
		}
		if l, ok := loop.(*handler.Loop); !ok || !l.BelowFloor {
			t.Errorf("LLM_BELOW_FLOOR=%s did not enable the knob", v)
		}
	}
	llmEnv(t)
	t.Setenv("LLM_PROVIDER", "fake")
	t.Setenv("LLM_BELOW_FLOOR", "yes please")
	if _, _, _, err := answeringBoot(t); err == nil {
		t.Errorf("LLM_BELOW_FLOOR=%q was accepted", "yes please")
	}
}

func TestTheLoopsIntegerKnobsAreValidatedAtBoot(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"LLM_MAX_CONCURRENT", "0"},
		{"LLM_MAX_CONCURRENT", "-1"},
		{"LLM_MAX_CONCURRENT", "2O"}, // a letter O, the RETRIEVAL_CANDIDATES=4O shape
		{"LLM_TOKENS_PER_HOUR", "0"},
		{"LLM_TOKENS_PER_HOUR", "not a number"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			llmEnv(t)
			t.Setenv("LLM_PROVIDER", "fake")
			t.Setenv(tc.key, tc.value)
			if _, _, _, err := answeringBoot(t); err == nil {
				t.Errorf("booted with %s=%s", tc.key, tc.value)
			} else if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("the boot error does not name %s: %v", tc.key, err)
			}
		})
	}
}

// The bounds an operator is running under are in the boot log, so the published
// worst case can be checked against the process rather than against the README
// alone.
func TestTheBoundsAreInTheBootLog(t *testing.T) {
	llmEnv(t)
	t.Setenv("LLM_PROVIDER", "fake")
	loop, def, logged, err := answeringBoot(t)
	if err != nil {
		t.Fatal(err)
	}
	if loop == nil {
		t.Fatal("no loop built for LLM_PROVIDER=fake")
	}
	if def != "extractive" {
		t.Errorf("ANSWER_DEFAULT is %q with a provider configured, want extractive", def)
	}
	for _, want := range []string{
		`"worst_case_tokens":13500`, `"max_steps":6`, `"max_tool_calls":12`,
		`"llm_tokens_per_hour":200000`, `"llm_max_concurrent":2`,
		"per process",
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the boot log does not carry %q: %s", want, logged)
		}
	}
}

// The client's own timeout must be LONGER than the loop's whole deadline.
//
// The loop's context is what should end a call; a client timeout shorter than
// it would end one first and report a client error where the trace should say
// `deadline`. Found by the whole-branch sweep: shortening it to 5s survived
// everything, because nothing related the two numbers.
func TestTheClientTimeoutOutlastsTheLoopDeadline(t *testing.T) {
	b := agent.DefaultBounds()
	if llmClientTimeout <= b.Deadline {
		t.Errorf("llmClientTimeout is %v and the loop's deadline is %v: the client would end a call first and report the wrong reason",
			llmClientTimeout, b.Deadline)
	}
}

// liveStore is deadStore with a Postgres that answers, so a readiness failure
// below can only be the dependency beside it.
type liveStore struct{ deadStore }

func (liveStore) Ping(context.Context) error { return nil }

// deadEmbedder is what an Ollama that has stopped looks like from here.
type deadEmbedder struct{}

func (deadEmbedder) Model() string { return "dead" }
func (deadEmbedder) Dim() int      { return store.EmbeddingDim }
func (deadEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("ollama is down")
}

// /ready is the load balancer's question, and retrieval embeds the QUESTION on
// every /search and every /ask — so a gateway whose embedder has gone can
// answer neither, and before this it kept saying 200 while the load balancer
// kept sending traffic. newRetriever's own comment makes boot depend on the
// embedder for exactly that reason; after boot nothing did.
func TestReadinessReflectsTheEmbedderAndNotOnlyPostgres(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/x")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	retriever := func(e embed.Embedder) *rag.Retriever {
		return &rag.Retriever{Store: deadStore{pool}, Emb: e, Mode: rag.ModeHybrid,
			Fusion: rag.DefaultParams(), Candidates: 40, Floor: rag.DefaultFloor()}
	}

	var logged bytes.Buffer
	down := newServer(zerolog.New(&logged).Level(zerolog.InfoLevel), liveStore{deadStore{pool}},
		retriever(deadEmbedder{}), rag.DefaultBudget(), nil, "extractive")
	if rec := serve(down, http.MethodGet, "/ready", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/ready: want 503 with the embedder down, got %d", rec.Code)
	}
	if !strings.Contains(logged.String(), `"dependency":"embedder"`) {
		t.Errorf("the log does not say which dependency went: %s", logged.String())
	}
	// Liveness is not readiness: killing this task would not bring Ollama back,
	// and a container health check on /ready turns one outage into a rolling
	// restart of every task.
	if rec := serve(down, http.MethodGet, "/health", ""); rec.Code != http.StatusOK {
		t.Errorf("/health: want 200 with the embedder down, got %d", rec.Code)
	}

	// The other direction, so a readiness that is simply always false fails too.
	up := newServer(zerolog.Nop(), liveStore{deadStore{pool}},
		retriever(embed.NewFake(store.EmbeddingDim)), rag.DefaultBudget(), nil, "extractive")
	if rec := serve(up, http.MethodGet, "/ready", ""); rec.Code != http.StatusOK {
		t.Errorf("/ready: want 200 with both dependencies answering, got %d", rec.Code)
	}
}
