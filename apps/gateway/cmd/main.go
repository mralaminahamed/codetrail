// Command gateway serves codetrail's HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"strconv"
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
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/logger"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
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

// newHandler builds the API handler main serves from. A function so a test can
// pin the wiring: every field here is one main could silently forget, and a
// zero-value logger discards without complaining.
func newHandler(log zerolog.Logger, q handler.Enqueuer) *handler.Handler {
	return &handler.Handler{Policy: admit.NewPolicy(allowedHosts()), Jobs: q, Log: log}
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
	mode, err := rag.ParseMode(config.Get("RETRIEVAL_MODE", string(rag.ModeHybrid)))
	if err != nil {
		return nil, err
	}
	// Fuse takes k as an argument and guards nothing: k = -1 makes 1/(k+1) an
	// infinity and k <= -2 inverts the ranking, both silently, and config.GetInt
	// parses "-1" happily. 60 is the constant from the paper the method comes
	// from and is not measured against this corpus.
	k := config.GetInt("RETRIEVAL_RRF_K", 60)
	if k < 0 {
		return nil, fmt.Errorf("RETRIEVAL_RRF_K must not be negative, got %d", k)
	}
	// 40 per arm, which is pgvector 0.8.6's own hnsw.ef_search default on the
	// pinned image — read from pg_settings.boot_val, not assumed. Asking the
	// ANN index for more rows than ef_search degrades recall with no error,
	// and hnsw.iterative_scan is off by default, so nothing compensates.
	candidates := config.GetInt("RETRIEVAL_CANDIDATES", 40)
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
		K: k, Candidates: candidates, Split: split, Floor: floor,
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
// Parsed here rather than through config, because config.GetInt answers its
// default for anything it cannot parse and a floor that silently reverts to -1
// on a typo is a filter an operator believes is running.
//
// Calibrated is false whatever the value: the flag says *codetrail* measured
// this number, and spec:315 puts that in P6. An operator's own number is still
// not one this project has evidence for.
func scoreFloor() (rag.Floor, error) {
	f := rag.DefaultFloor()
	if v := config.Get("ANSWER_SCORE_FLOOR", ""); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return rag.Floor{}, fmt.Errorf("ANSWER_SCORE_FLOOR must be a number, got %q", v)
		}
		f.Value = n
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
func newServer(log zerolog.Logger, st storeHandle) *echo.Echo {
	return newRouter(readinessFor(log, st).Ready, newHandler(log, jobs.New(st.Pool())))
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

	// Nothing serves from it yet — the read endpoints are the next task — but
	// boot depends on it now, because a gateway that starts without a working
	// embedder can only fail one request at a time.
	if _, err := newRetriever(ctx, log, st); err != nil {
		log.Fatal().Err(err).Msg("bad retrieval config")
	}

	e := newServer(log, st)

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
