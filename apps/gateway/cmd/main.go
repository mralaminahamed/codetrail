// Command gateway serves codetrail's HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/apps/gateway/internal/handler"
	"github.com/mralaminahamed/codetrail/apps/gateway/internal/server"
	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/agent"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/health"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/llm"
	"github.com/mralaminahamed/codetrail/packages/shared/logger"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

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
	return config.GetList("ALLOWED_HOSTS", admit.DefaultHosts)
}

// newHandler builds the API handler main serves from. A function so a test can
// pin the wiring: every field here is one main could silently forget, and a
// zero-value logger discards without complaining.
//
// The floor is copied off the retriever rather than read from the environment a
// second time: two parses of ANSWER_SCORE_FLOOR could disagree, and then the
// number in the payload would not be the number the decision used.
func newHandler(log zerolog.Logger, q handler.Enqueuer, rd handler.Reader, r *rag.Retriever,
	b rag.Budget, loop handler.Answerer, answerDefault string,
) *handler.Handler {
	return &handler.Handler{
		Policy: admit.NewPolicy(allowedHosts()), Jobs: q,
		Repos: rd, Rag: r, Floor: r.Floor, Budget: b, Now: time.Now, Log: log,
		LLM: loop, AnswerDefault: answerDefault,
	}
}

// llmClientTimeout is one HTTP request's own budget, deliberately LONGER than
// the loop's whole deadline: the loop's context is what should end a call, and
// a client timeout shorter than it would end one first and report the wrong
// reason.
const llmClientTimeout = 90 * time.Second

// answering builds the loop, or returns nil when there is no provider.
//
// nil is not a degraded state. LLM_PROVIDER defaults to none, and with none no
// client is constructed, no key is read and nothing is dialled — spec:230-232
// makes the extractive path the default and says a public demo costs nothing
// per visitor.
//
// ANSWER_DEFAULT defaults to extractive EVEN WITH A PROVIDER CONFIGURED. A
// public demo therefore runs LLM_PROVIDER=anthropic with
// ANSWER_DEFAULT=extractive: a visitor gets the free path, and a caller who
// spends has to say so.
func answering(log zerolog.Logger, rd handler.Reader, r *rag.Retriever) (handler.Answerer, string, error) {
	def := config.Get("ANSWER_DEFAULT", "extractive")
	if def != "extractive" && def != "llm" {
		return nil, "", fmt.Errorf("ANSWER_DEFAULT must be extractive or llm, got %q", def)
	}
	m, err := llm.FromEnv(llmClientTimeout)
	if err != nil {
		return nil, "", err
	}
	if m == nil {
		// A setting an operator believes is in force and is not: refused at
		// boot, like every other knob in this codebase.
		if def == "llm" {
			return nil, "", errors.New("ANSWER_DEFAULT=llm with LLM_PROVIDER=none: there is no model to answer with")
		}
		log.Info().Str("answer_default", def).
			Msg("no model provider configured; every answer is extractive and nothing is a degradation")
		return nil, def, nil
	}
	belowFloor, err := boolEnv("LLM_BELOW_FLOOR", "false")
	if err != nil {
		return nil, "", err
	}
	concurrent, err := config.GetInt("LLM_MAX_CONCURRENT", 2)
	if err != nil {
		return nil, "", err
	}
	if concurrent < 1 {
		return nil, "", fmt.Errorf("LLM_MAX_CONCURRENT must be positive, got %d", concurrent)
	}
	// 200,000 is roughly fifteen worst-case requests an hour per process. NOT
	// MEASURED — chosen to be obviously finite, and named as such. Per process:
	// with n replicas the real ceiling is n times this, and the only control
	// that is actually a ceiling is the provider's own account spend cap.
	perHour, err := config.GetInt("LLM_TOKENS_PER_HOUR", 200_000)
	if err != nil {
		return nil, "", err
	}
	if perHour < 1 {
		return nil, "", fmt.Errorf("LLM_TOKENS_PER_HOUR must be positive, got %d", perHour)
	}
	b := agent.DefaultBounds()
	if err := b.Validate(); err != nil {
		return nil, "", err
	}
	lim := agent.DefaultToolLimits()
	log.Info().
		Str("llm_model", m.Name()).Str("answer_default", def).
		Bool("llm_below_floor", belowFloor).Int("llm_max_concurrent", concurrent).
		Int("llm_tokens_per_hour", perHour).
		Int("max_steps", b.MaxSteps).Int("max_tool_calls", b.MaxToolCalls).
		Int("worst_case_tokens", b.MaxInputTokens+b.MaxOutputTokens).
		Msg("answering loop configured; the token budget is per process and is not shared across replicas")
	return handler.NewLoop(m, corpusOf(rd, r), b, lim, belowFloor, concurrent, perHour, time.Now), def, nil
}

// boolEnv parses rather than comparing against "true". The house rule, stated
// three times in this codebase: a knob an operator believes is in force and is
// not is the shape of bug this project has already shipped.
func boolEnv(key, def string) (bool, error) {
	v := config.Get(key, def)
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", key, v)
	}
	return b, nil
}

// answerBudget reads what one answer may hold. Both knobs are validated rather
// than clamped, for the reason the read endpoints refuse an out-of-range limit:
// a zero budget is a misconfiguration that would answer every question with an
// empty string and no error anywhere.
func answerBudget() (rag.Budget, error) {
	b := rag.DefaultBudget()
	var err error
	if b.MaxSpans, err = config.GetInt("ANSWER_MAX_SPANS", b.MaxSpans); err != nil {
		return rag.Budget{}, err
	}
	if b.MaxChars, err = config.GetInt("ANSWER_MAX_CHARS", b.MaxChars); err != nil {
		return rag.Budget{}, err
	}
	if err := b.Validate(); err != nil {
		return rag.Budget{}, err
	}
	return b, nil
}

// queryEmbedTimeout bounds one embed call on the read path. The indexer hands
// its embedder the job deadline; a query has no such budget, and a hung model
// would otherwise hold a request open for as long as it liked.
const queryEmbedTimeout = 15 * time.Second

// newRetriever builds what the read path retrieves with, and refuses to boot on
// any knob it cannot serve.
//
// The gateway needs an embedder because retrieval embeds the *question*, which
// makes EMBED_PROVIDER, EMBED_MODEL, EMBED_DIM and OLLAMA_URL gateway
// configuration for the first time. It boots-or-dies on them exactly as the
// indexer does, and the cost is real and stated: with EMBED_PROVIDER=ollama and
// Ollama down, submission and job polling go down with retrieval. Parity is
// chosen over booting into a degraded mode because a degraded mode is the
// silent downgrade spec §8 calls the failure that costs a week, and there is no
// state in this codebase for one. The embedder is built even in lexical mode,
// where nothing calls it, for the same reason: one boot contract, not three.
//
// The embedder is built last, after every knob that costs nothing to check, so
// a typo in RETRIEVAL_MODE fails before a round trip rather than after it.
func newRetriever(ctx context.Context, log zerolog.Logger, st rag.Searcher) (*rag.Retriever, error) {
	// vector, not hybrid, and the default moved on a measurement rather than on
	// a preference. P6 ran all three modes over one corpus (google/uuid @
	// 2d3c2a9, 74 cases, live nomic-embed-text) and the AST arm's MRR was
	// vector 0.7492, hybrid 0.4023, lexical 0.1637 — a gap of 0.347 against a
	// pre-registered threshold of max(2σ, 0.02) = 0.104.
	//
	// It is ONE CORPUS, and the golden set is doc-comment prose, which is close
	// to the vector arm's best case and the lexical arm's worst. The mechanism
	// stays shipped and RETRIEVAL_MODE still takes all three values. The README
	// carries the numbers and the limits; do not restate the result here
	// without them.
	mode, err := rag.ParseMode(config.Get("RETRIEVAL_MODE", string(rag.ModeVector)))
	if err != nil {
		return nil, err
	}
	// Fuse takes k as an argument and guards nothing: k = -1 makes 1/(k+1) an
	// infinity and k <= -2 inverts the ranking, both silently, and config.GetInt
	// parses "-1" happily. 60 is the constant from the paper the method comes
	// from and is not measured against this corpus.
	k, err := config.GetInt("RETRIEVAL_RRF_K", 60)
	if err != nil {
		return nil, err
	}
	if k < 0 {
		return nil, fmt.Errorf("RETRIEVAL_RRF_K must not be negative, got %d", k)
	}
	// 40 per arm, which is pgvector 0.8.6's own hnsw.ef_search default on the
	// pinned image — read from pg_settings.boot_val, not assumed. Asking the
	// ANN index for more rows than ef_search degrades recall with no error,
	// and hnsw.iterative_scan is off by default, so nothing compensates.
	candidates, err := config.GetInt("RETRIEVAL_CANDIDATES", 40)
	if err != nil {
		return nil, err
	}
	if candidates < 1 {
		return nil, fmt.Errorf("RETRIEVAL_CANDIDATES must be positive, got %d", candidates)
	}
	split, err := splitIdentifiers()
	if err != nil {
		return nil, err
	}
	floor, err := scoreFloor()
	if err != nil {
		return nil, err
	}
	emb, err := embed.FromEnv(ctx, store.EmbeddingDim, store.CheckDim, queryEmbedTimeout)
	if err != nil {
		return nil, err
	}

	// Both gauges at boot, so a dashboard shows the floor — and that nobody has
	// measured it — before the first query rather than after it.
	metrics.SetFloor(floor.Value, floor.Calibrated)
	log.Info().
		Str("mode", string(mode)).Int("rrf_k", k).Int("candidates", candidates).
		Bool("split_identifiers", split).
		Float64("score_floor", floor.Value).Bool("floor_calibrated", floor.Calibrated).
		Str("embed_model", emb.Model()).Int("embed_dim", emb.Dim()).
		Msg("retrieval configured; the score floor is not calibrated, its value is measured in P6")

	return &rag.Retriever{
		Store: st, Emb: emb, Mode: mode,
		Fusion:     rag.Params{K: k, WVector: 1, WLexical: 1},
		Candidates: candidates, Split: split, Floor: floor,
	}, nil
}

// splitIdentifiers reports whether a query's camel-case parts are added to its
// terms. Parsed rather than compared against "true", like STRIP_DOC_COMMENTS:
// LEXICAL_SPLIT_IDENTIFIERS=yes would otherwise read as false and quietly
// narrow every lexical query.
func splitIdentifiers() (bool, error) {
	v := config.Get("LEXICAL_SPLIT_IDENTIFIERS", "true")
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("LEXICAL_SPLIT_IDENTIFIERS must be a boolean, got %q", v)
	}
	return b, nil
}

// scoreFloor reads ANSWER_SCORE_FLOOR and refuses what a cosine similarity
// cannot produce, the way chunk.Options and walk.Limits refuse their own.
//
// Parsed here rather than through config, which reads integers only. The
// refusal is config.GetInt's now too, and for the same reason: a floor that
// silently reverted to -1 on a typo is a filter an operator believes is
// running.
//
// Calibrated is false whatever the value: the flag says *codetrail* measured
// this number, and spec:315 puts that in P6. An operator's own number is still
// not one this project has evidence for.
//
// Written out rather than inherited from the default. Overwriting only Value
// is correct exactly while DefaultFloor().Calibrated is false, and the moment
// P6 measures a floor it silently starts labelling a hand-typed number as
// measured — a gauge, a boot log line and a user-visible refusal payload all
// saying codetrail has evidence it does not have.
func scoreFloor() (rag.Floor, error) {
	f := rag.DefaultFloor()
	if v := config.Get("ANSWER_SCORE_FLOOR", ""); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return rag.Floor{}, fmt.Errorf("ANSWER_SCORE_FLOOR must be a number, got %q", v)
		}
		f = rag.Floor{Value: n, Calibrated: false}
	}
	if err := f.Validate(); err != nil {
		return rag.Floor{}, err
	}
	return f, nil
}

// newServer assembles what the binary serves: probes, router, policy, queue and
// logger. A function so a test can drive the assembly, because the mistakes
// here are silent: server.New's probes with no API mounted behind them answer
// /health and /ready and nothing anyone asked for.
//
// main's own body is reachable too — openStore is replaceable, so a test can
// run main() with no database. None does: under -coverprofile every statement
// in main is count 0, and mutating the PORT default, the DSN default or the
// 10s shutdown timeout leaves the suite green. So the logger is not the one
// argument left uncovered there — the whole body is. It is only the one that
// was mutated and recorded as surviving.
//
// Closing that gap means running main() under test: a self-directed SIGTERM to
// make it return, and an os.Stdout swap to see what it logged. That is heavier
// and flakier machinery than anything else in this suite. The body is uncovered
// by judgement, not because it cannot be reached.
func newServer(log zerolog.Logger, st storeHandle, r *rag.Retriever, b rag.Budget,
	loop handler.Answerer, answerDefault string,
) *echo.Echo {
	return newRouter(health.NewReadiness(log, st.Ping, embedderDep(r)).Ready,
		newHandler(log, jobs.New(st.Pool()), st, r, b, loop, answerDefault))
}

// embedderDep keeps the boot probe running.
//
// newRetriever's own comment argues boot must depend on the embedder because a
// gateway that starts without a working one can only fail one request at a
// time — and after boot it did exactly that and nothing noticed: Ollama dies,
// every /search and /ask 500s because retrieval embeds the question, and /ready
// answers 200 while the load balancer keeps sending traffic.
//
// embed.Probe, not a second check of our own: two definitions of "the embedder
// works" agree on the day they are written.
func embedderDep(r *rag.Retriever) health.Dep {
	return health.Dep{Name: "embedder", Check: func(ctx context.Context) error {
		return embed.Probe(ctx, r.Emb)
	}}
}

// corpus is what the four tools read through: the same Reader the endpoints use
// and the same Retriever the search route uses.
//
// The tools call the STORE, not the gateway's own HTTP API. Routing a tool
// through POST /api/repos/:repo/search would mean the public process making an
// outbound request whose path and body a model influences, inside the process
// that is already the public one. The cost is that a tool and its endpoint are
// two code paths that can drift, and Task 10's live test is what keeps them
// honest.
type corpus struct {
	handler.Reader
	rag *rag.Retriever
}

func (c corpus) Search(ctx context.Context, repoID, q string, limit int) (rag.Result, error) {
	return c.rag.Search(ctx, repoID, q, limit)
}

func corpusOf(rd handler.Reader, r *rag.Retriever) agent.Corpus { return corpus{Reader: rd, rag: r} }

// probeMode is the container health check. See health.Probe: the image is
// distroless, so there is no shell to run a curl in.
var probeMode = flag.Bool("probe", false, "check /health on this process's own port, then exit 0 or 1")

// listenAddr is where this process serves, read in one place so -probe and the
// server cannot disagree about the port.
func listenAddr() string { return ":" + config.Get("PORT", "8080") }

func main() {
	flag.Parse()
	if *probeMode {
		if err := health.Probe(listenAddr()); err != nil {
			fmt.Fprintln(os.Stderr, "probe:", err)
			os.Exit(1)
		}
		return
	}

	log := logger.New("gateway")
	ctx := context.Background()

	dsn := config.Get("DATABASE_URL", "postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable")
	st, err := openStore(ctx, dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("connect postgres")
	}
	defer st.Close()
	log.Info().Msg("postgres ready, schema up to date")

	// Boot depends on the embedder, because a gateway that starts without a
	// working one can only fail one request at a time.
	ret, err := newRetriever(ctx, log, st)
	if err != nil {
		log.Fatal().Err(err).Msg("bad retrieval config")
	}
	budget, err := answerBudget()
	if err != nil {
		log.Fatal().Err(err).Msg("bad answer budget")
	}
	log.Info().Int("answer_max_spans", budget.MaxSpans).Int("answer_max_chars", budget.MaxChars).
		Msg("answer budget configured; spans are dropped whole, never truncated")
	loop, answerDefault, err := answering(log, st, ret)
	if err != nil {
		log.Fatal().Err(err).Msg("bad answering config")
	}

	e := newServer(log, st, ret, budget, loop, answerDefault)

	addr := listenAddr()
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
