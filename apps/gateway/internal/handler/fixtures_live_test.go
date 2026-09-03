//go:build live

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// The console's fixtures, written by the handlers that serve them.
//
// A hand-written fixture is a guess about the API that typechecks against a
// hand-written type: two guesses agreeing with each other. Everything here is
// emitted from the shipped response structs over a live Postgres, so a change
// to a response shape fails this test rather than being discovered by a
// console that renders the old one.
//
// UPDATE_CONSOLE_FIXTURES=1 writes; without it this compares and fails naming
// the file. An environment variable rather than the -update flag the plan
// named: `go test -tags=live ./...` passes a flag to every package, and the
// ones that do not declare it abort with "flag provided but not defined".
var updatingFixtures = os.Getenv("UPDATE_CONSOLE_FIXTURES") != ""

const fixtureDir = "../../../console/src/api/fixtures"

// consoleNow dates every citation and staleness note below. Pinned through
// Handler.Now, which is the seam that field exists for.
var consoleNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// consoleRequestID is what the 500 fixture quotes. echo's RequestID middleware
// honours an inbound X-Request-Id, which is the only seam that makes a
// generated id reproducible.
const consoleRequestID = "01JQ0FIXTURE0000000000REQ"

// Class is how far the shipped stack carried a fixture, and it is recorded per
// file in NOTES.md because a fixture the stack cannot produce is a fixture the
// guard cannot guard.
//
//	a — the real *rag.Retriever and the real *store.Store, end to end.
//	b — the shipped Handler with one interface stubbed (Retriever, Reader or
//	    Enqueuer). A stub supplies a rag.Result or a store row; never a body.
//	c — a hand-written literal. Exactly one, and it says why beside itself.

// ---------------------------------------------------------------- the corpus

// consoleIndex writes the committed indexer fixture tree into one repository at
// a chosen remote, ref and commit.
//
// A copy of read_live_test.go's indexFixture rather than a call to it: that one
// hard-codes a github.com remote, and the whole point of citation-codeberg.json
// and citation-nolink.json is that the remote decides whether a permalink
// exists at all. It writes through the same three shared packages the indexer
// writes with — chunk, embed.FromEnv and store — so a span's identity is the
// identity production gives it.
func consoleIndex(t *testing.T, st *store.Store, remote, ref, commit string, strategy chunk.Strategy) models.Repo {
	t.Helper()
	ctx := context.Background()

	emb, err := embed.FromEnv(ctx, store.EmbeddingDim, store.CheckDim, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opt := chunk.Defaults()
	opt.Strategy = strategy

	repo := models.Repo{ID: store.RepoID(remote, commit), Remote: remote, Ref: ref,
		Commit: commit, SizeBytes: 4096}

	var files []models.File
	var spans []store.EmbeddedSpan
	err = filepath.WalkDir(fixtureRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(fixtureRoot, p)
		if err != nil {
			return err
		}
		path := filepath.ToSlash(strings.TrimSuffix(rel, ".txt"))
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		lang := langByExt[filepath.Ext(path)]
		row := models.File{ID: store.FileID(repo.ID, path), RepoID: repo.ID, Path: path,
			Blob: store.Digest(string(body))[:40], Lang: lang, Lines: strings.Count(string(body), "\n")}
		files = append(files, row)
		if lang == "" {
			return nil
		}
		cs, _, cerr := chunk.Chunks(path, body, opt)
		if cerr != nil {
			return cerr
		}
		for _, c := range cs {
			digest := store.Digest(c.Text)
			spans = append(spans, store.EmbeddedSpan{Span: models.Span{
				ID: store.SpanID(repo.ID, path, c.StartLine, c.EndLine, digest), RepoID: repo.ID,
				FileID: row.ID, Path: path, Kind: c.Kind, Symbol: c.Symbol,
				StartLine: c.StartLine, EndLine: c.EndLine, Text: c.Text, Digest: digest,
			}})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	texts := make([]string, len(spans))
	for i, sp := range spans {
		texts[i] = sp.Text
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range vecs {
		spans[i].Embedding = vecs[i]
	}
	if err := st.PutRepo(ctx, repo, files); err != nil {
		t.Fatal(err)
	}
	if len(spans) > 0 {
		if err := st.PutSpans(ctx, repo.ID, spans, emb.Model(), emb.Dim()); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// pinRepoClock is the second half of the timestamp seam, and the reason there
// has to be one: PutRepo writes indexed_at and last_queried_at from now() in
// SQL (packages/shared/store/repos.go), which no Go field reaches. Handler.Now
// pins the clock a note is computed *against*; this pins the row the note is
// computed *from*. Normalising either out of the fixture afterwards would let
// the staleness sentence drift without this guard noticing, which is the one
// drift it exists to catch.
func pinRepoClock(t *testing.T, st *store.Store, repoID string, indexedAt, lastUsedAt time.Time) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE repos SET indexed_at = $2, last_queried_at = $3 WHERE id = $1`,
		repoID, indexedAt, lastUsedAt); err != nil {
		t.Fatal(err)
	}
}

// ------------------------------------------------------------------- writing

func writeFixture(t *testing.T, name string, raw []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(raw), "", "  "); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", name, err, raw)
	}
	pretty.WriteByte('\n')

	path := filepath.Join(fixtureDir, name)
	if updatingFixtures {
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("fixtures/%s: %v — re-run with UPDATE_CONSOLE_FIXTURES=1", name, err)
		return
	}
	if !bytes.Equal(have, pretty.Bytes()) {
		t.Errorf("fixtures/%s differs from what the handlers now produce.\n"+
			"--- committed\n%s\n--- now\n%s", name, have, pretty.Bytes())
	}
}

// emit writes one response and asserts the status it was served under, because
// the status is half of what the console branches on and no fixture file
// records it.
func emit(t *testing.T, name string, want int, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("%s: status %d, want %d: %s", name, rec.Code, want, rec.Body)
	}
	writeFixture(t, name, rec.Body.Bytes())
	return rec.Body.Bytes()
}

// consoleGet and consolePost carry a fixed X-Request-Id so the 500 body's
// request_id is reproducible, and mount() carries the middleware server.New
// does.
func consoleGet(h *Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(echo.HeaderXRequestID, consoleRequestID)
	rec := httptest.NewRecorder()
	mount(h).ServeHTTP(rec, req)
	return rec
}

func consolePost(h *Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(echoContentType, echoJSON)
	req.Header.Set(echo.HeaderXRequestID, consoleRequestID)
	rec := httptest.NewRecorder()
	mount(h).ServeHTTP(rec, req)
	return rec
}

// -------------------------------------------------------------------- stubs

// stubReader is the shipped *store.Store with named reads replaced. Class (b):
// the response still comes out of callersOfSymbol, citer, symbolOf and
// rag.NewCitation — a stub supplies store rows, never a body.
//
// It exists because the committed indexer fixture tree declares no identifier
// twice, contains exactly one resolved call edge and no unresolved call to any
// name it also defines. So the payloads fixture rules 2 and 5 require — two
// definitions of one name, a caller row whose citation is null, a precise and
// an approximate list sharing a symbol name — are not producible from it by
// any configuration. Recorded rather than papered over.
type stubReader struct {
	Reader
	defs       []models.Symbol
	callers    []store.Caller
	approx     []store.Approximate
	approxErr  error
	statsErr   error
	callersErr error
}

func (s *stubReader) Definitions(ctx context.Context, repoID, name, pkg string, suffix bool, limit int) ([]models.Symbol, error) {
	if s.defs == nil {
		return s.Reader.Definitions(ctx, repoID, name, pkg, suffix, limit)
	}
	return s.defs, nil
}

func (s *stubReader) CallersOf(ctx context.Context, repoID, symbolID string, depth, limit int) ([]store.Caller, error) {
	if s.callersErr != nil {
		return nil, s.callersErr
	}
	if s.callers == nil {
		return s.Reader.CallersOf(ctx, repoID, symbolID, depth, limit)
	}
	return s.callers, nil
}

func (s *stubReader) ApproximateCallersOf(ctx context.Context, repoID, name string, limit int) ([]store.Approximate, error) {
	if s.approxErr != nil {
		return nil, s.approxErr
	}
	if s.approx == nil {
		return s.Reader.ApproximateCallersOf(ctx, repoID, name, limit)
	}
	return s.approx, nil
}

func (s *stubReader) RepoStats(ctx context.Context, repoID string) (store.Stats, error) {
	if s.statsErr != nil {
		return store.Stats{}, s.statsErr
	}
	return s.Reader.RepoStats(ctx, repoID)
}

// stubEnqueuer fails, so the 500 body's request_id has something to be about
// on a route whose store read cannot be made to fail without breaking the
// reads before it.
type stubEnqueuer struct{ err error }

func (s stubEnqueuer) Enqueue(context.Context, string, string) (jobs.Job, error) {
	return jobs.Job{}, s.err
}
func (s stubEnqueuer) Get(context.Context, string) (jobs.Job, error) {
	return jobs.Job{}, s.err
}

// ------------------------------------------------------------------ handlers

// consoleHandler is the shipped composition with three things pinned: the
// clock, the floor, and the vector arm's candidate depth. candidates is
// RETRIEVAL_CANDIDATES, a supported process setting (apps/gateway/cmd/main.go),
// and search-hybrid.json needs a small one — see the assertion beside it.
func consoleHandler(t *testing.T, rd Reader, st *store.Store, mode rag.Mode, floor rag.Floor, candidates int) *Handler {
	t.Helper()
	emb, err := embed.FromEnv(context.Background(), store.EmbeddingDim, store.CheckDim, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{
		Policy: admit.NewPolicy(admit.DefaultHosts),
		Jobs:   jobs.New(st.Pool()),
		Repos:  rd,
		Rag: &rag.Retriever{Store: st, Emb: emb, Mode: mode,
			K: 60, Candidates: candidates, Split: true, Floor: floor},
		Floor:  floor,
		Budget: rag.DefaultBudget(),
		Now:    func() time.Time { return consoleNow },
	}
}

// spanContaining is the rule the indexer links a symbol to a span with:
// containment of the declaration's first line, most specific first
// (apps/indexer/cmd/graph.go). Empty when nothing covers the line, which is
// the "this declaration has no span" state graph.go:37-39 keeps a key for.
func spanContaining(t *testing.T, st *store.Store, repoID, path string, line int) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `
		SELECT id FROM spans
		WHERE repo_id = $1 AND path = $2 AND start_line <= $3 AND end_line >= $3
		ORDER BY (end_line - start_line) ASC, id LIMIT 1`, repoID, path, line)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		return ""
	}
	var id string
	if err := rows.Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// ------------------------------------------------------------------ the emit

const (
	ghRemote      = "https://github.com/codetrail-live/zerolog"
	cbRemote      = "https://codeberg.org/codetrail-live/forgejo-demo"
	glRemote      = "https://gitlab.com/codetrail-live/thirdforge"
	movedRemote   = "https://github.com/codetrail-live/sampler"
	evictedRemote = "https://github.com/codetrail-live/evicted"
	emptyRemote   = "https://github.com/codetrail-live/emptycorpus"
	// A question drawn from the corpus, so the fake embedder — a hashed bag of
	// the same words — retrieves the spans that hold it.
	consoleQ = "how does Machine Push add n to the running total"
)

func TestConsoleFixturesMatchTheShippedHandlersLive(t *testing.T) {
	st := liveStore(t)
	ctx := context.Background()
	sha := func(s string) string { return store.Digest(s)[:40] }

	// The fourth source of nondeterminism, measured rather than reasoned
	// about: pgx decodes a timestamptz into time.Local and encoding/json
	// renders a time.Time with its location's offset, so the same instant from
	// the same handler emits "2026-08-31T18:00:00+06:00" on a +06 machine and
	// "2026-08-31T12:00:00Z" on CI's UTC runner. The drift guard would then
	// fail for a reason that is not a handler change.
	//
	// The instant is untouched; only the location it is rendered in is pinned.
	// That is deliberately not the same thing as normalising the timestamp
	// away, which would blind the guard to the staleness sentence — the one
	// drift it exists to catch.
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })

	main := consoleIndex(t, st, ghRemote, "main", sha("console-main"), chunk.StrategyAST)
	moved := consoleIndex(t, st, movedRemote, "main", sha("console-moved-old"), chunk.StrategyAST)
	newer := consoleIndex(t, st, movedRemote, "main", sha("console-moved-new"), chunk.StrategyAST)
	cb := consoleIndex(t, st, cbRemote, "main", sha("console-codeberg"), chunk.StrategyAST)
	gl := consoleIndex(t, st, glRemote, "main", sha("console-gitlab"), chunk.StrategyAST)
	evicted := consoleIndex(t, st, evictedRemote, "main", sha("console-evicted"), chunk.StrategyAST)

	// A corpus with files and no spans: the only way to reach spec §10's
	// no_spans refusal, which is one of the two reachable at the shipped floor.
	empty := models.Repo{ID: store.RepoID(emptyRemote, sha("console-empty")), Remote: emptyRemote,
		Ref: "main", Commit: sha("console-empty"), SizeBytes: 1024}
	if err := st.PutRepo(ctx, empty, []models.File{{
		ID: store.FileID(empty.ID, "README.md"), RepoID: empty.ID, Path: "README.md",
		Blob: sha("console-empty-readme"), Lang: "markdown", Lines: 3}}); err != nil {
		t.Fatal(err)
	}

	day := 24 * time.Hour
	pinRepoClock(t, st, main.ID, consoleNow.Add(-3*day), consoleNow.Add(-1*time.Hour))
	pinRepoClock(t, st, moved.ID, consoleNow.Add(-30*day), consoleNow.Add(-30*day))
	pinRepoClock(t, st, newer.ID, consoleNow.Add(-2*day), consoleNow.Add(-2*day))
	pinRepoClock(t, st, cb.ID, consoleNow.Add(-9*day), consoleNow.Add(-9*day))
	pinRepoClock(t, st, gl.ID, consoleNow.Add(-40*day), consoleNow.Add(-40*day))
	pinRepoClock(t, st, evicted.ID, consoleNow.Add(-99*day), consoleNow.Add(-99*day))
	pinRepoClock(t, st, empty.ID, consoleNow.Add(-5*day), consoleNow.Add(-50*day))

	hybrid := consoleHandler(t, st, st, rag.ModeHybrid, rag.DefaultFloor(), 40)

	// --- jobs. The id is rewritten because Enqueue mixes time.Now().UnixNano()
	// into it (packages/shared/jobs/jobs.go:92), which is a third source of
	// nondeterminism beside the two timestamp columns the plan named.
	//
	// Seeded terminal-first: Lease claims the oldest claimable row, so a pending
	// job enqueued before a leased one would be the row Lease takes.
	q := jobs.New(st.Pool())
	lease := func(want string) {
		t.Helper()
		j, ok, err := q.Lease(ctx, "console-fixtures", time.Minute)
		if err != nil || !ok {
			t.Fatalf("lease: ok=%v err=%v", ok, err)
		}
		if j.ID != want {
			t.Fatalf("leased %s, want %s", j.ID, want)
		}
	}

	leased := seedJob(t, st, "https://github.com/codetrail-live/leased", "console-job-leased")
	lease(leased)
	emit(t, "job-leased.json", http.StatusOK, consoleGet(hybrid, "/api/jobs/"+leased))

	done := seedJob(t, st, "https://github.com/codetrail-live/done", "console-job-done")
	lease(done)
	if err := q.Complete(ctx, done, "console-fixtures", main.ID); err != nil {
		t.Fatal(err)
	}
	emit(t, "job-done.json", http.StatusOK, consoleGet(hybrid, "/api/jobs/"+done))

	failed := seedJob(t, st, "https://github.com/codetrail-live/failed", "console-job-failed")
	lease(failed)
	// maxAttempts 1, so the first failure is terminal rather than a retry.
	if err := q.Fail(ctx, failed, "console-fixtures", "clone failed", 1); err != nil {
		t.Fatal(err)
	}
	emit(t, "job-failed.json", http.StatusOK, consoleGet(hybrid, "/api/jobs/"+failed))

	pending := seedJob(t, st, "https://github.com/codetrail-live/pending", "console-job-pending")
	emit(t, "job-pending.json", http.StatusOK, consoleGet(hybrid, "/api/jobs/"+pending))

	// --- the four POST /api/repos rejections. Two share rule "form" and differ
	// only in detail, which is the pair that proves the console renders detail.
	emit(t, "error-400-scheme.json", http.StatusBadRequest,
		consolePost(hybrid, "/api/repos", `{"remote":"http://github.com/rs/zerolog"}`))
	emit(t, "error-400-host.json", http.StatusBadRequest,
		consolePost(hybrid, "/api/repos", `{"remote":"https://example.com/rs/zerolog"}`))
	emit(t, "error-400-form-path.json", http.StatusBadRequest,
		consolePost(hybrid, "/api/repos", `{"remote":"https://github.com/rs/zerolog/tree/master"}`))
	emit(t, "error-400-form-ref.json", http.StatusBadRequest,
		consolePost(hybrid, "/api/repos", `{"remote":"https://github.com/rs/zerolog","ref":"--upload-pack=x"}`))

	// --- the symbol graph. Seeded before the repo view is read, because
	// getRepo reports symbols, edges, edges_resolved and edges_syntactic and a
	// repo.json emitted over an unseeded graph carries four zeroes — which is
	// a fixture that cannot tell a console rendering the counts from one
	// rendering nothing. Every symbol below is a real declaration in the
	// committed fixture tree and every resolved edge sits on a line that really
	// holds that call; see NOTES.md for what the corpus cannot produce.
	push, total, table := seedConsoleGraph(t, st, main)
	t.Logf("big.go:2 containment span = %q (empty means the declaration has no span)", table.SpanID)

	// --- repositories and spans
	emit(t, "repo.json", http.StatusOK, consoleGet(hybrid, "/api/repos/"+main.ID))
	emit(t, "repo-stale.json", http.StatusOK, consoleGet(hybrid, "/api/repos/"+moved.ID))

	pushSpan := spanContaining(t, st, main.ID, "calc/calc.go", 19)
	if pushSpan == "" {
		t.Fatal("calc/calc.go:19 has no span; the AST corpus is not what this emitter assumes")
	}
	emit(t, "span.json", http.StatusOK, consoleGet(hybrid, "/api/repos/"+main.ID+"/spans/"+pushSpan))

	cbSpan := spanContaining(t, st, cb.ID, "calc/calc.go", 19)
	cbBody := emit(t, "citation-codeberg.json", http.StatusOK,
		consoleGet(hybrid, "/api/repos/"+cb.ID+"/spans/"+cbSpan))
	mustContain(t, "citation-codeberg.json", cbBody, "/src/commit/")

	glSpan := spanContaining(t, st, gl.ID, "calc/calc.go", 19)
	glBody := emit(t, "citation-nolink.json", http.StatusOK,
		consoleGet(hybrid, "/api/repos/"+gl.ID+"/spans/"+glSpan))
	mustContain(t, "citation-nolink.json", glBody, `"permalink":""`)

	// --- search. Two fixtures, one per mode, because no single response
	// carries both nullable-score shapes: in lexical-only mode VectorRan is
	// false so top_score and every vector_score are null, and in hybrid mode
	// top_score is a number and only the spans the vector arm missed are null.
	narrow := consoleHandler(t, st, st, rag.ModeHybrid, rag.DefaultFloor(), 2)
	hy := emit(t, "search-hybrid.json", http.StatusOK,
		consolePost(narrow, "/api/repos/"+main.ID+"/search", `{"q":"`+consoleQ+`","limit":5}`))
	assertHybridScores(t, hy)

	lexical := consoleHandler(t, st, st, rag.ModeLexical, rag.DefaultFloor(), 40)
	lx := emit(t, "search-lexical.json", http.StatusOK,
		consolePost(lexical, "/api/repos/"+main.ID+"/search", `{"q":"`+consoleQ+`","limit":5}`))
	assertLexicalScores(t, lx)

	// --- ask
	emit(t, "ask-answered.json", http.StatusOK,
		consolePost(hybrid, "/api/repos/"+main.ID+"/ask", `{"q":"`+consoleQ+`","limit":3}`))
	emit(t, "ask-refused-no-spans.json", http.StatusOK,
		consolePost(hybrid, "/api/repos/"+empty.ID+"/ask", `{"q":"`+consoleQ+`"}`))
	emit(t, "ask-refused-no-spans-lexical.json", http.StatusOK,
		consolePost(lexical, "/api/repos/"+empty.ID+"/ask", `{"q":"`+consoleQ+`"}`))

	// below_floor needs ANSWER_SCORE_FLOOR set: at the shipped default of -1 it
	// is unreachable, because Decide refuses when top < f.Value and a cosine
	// similarity is never below -1 (decide.go:25, decide.go:86).
	floored := consoleHandler(t, st, st, rag.ModeHybrid, rag.Floor{Value: 0.99}, 40)
	emit(t, "ask-refused-below-floor.json", http.StatusOK,
		consolePost(floored, "/api/repos/"+main.ID+"/ask", `{"q":"`+consoleQ+`"}`))

	// Class (b). Assemble's "the span did not travel" branch is dead code under
	// the shipped retriever — spansOf fills its map from the very hits it is
	// handed (retrieve.go:190-204) — so {"answer":"","citations":null} is a
	// shape a client must handle and no query can produce.
	emptyAssemble := consoleHandler(t, st, st, rag.ModeHybrid, rag.DefaultFloor(), 40)
	emptyAssemble.Rag = &fakeRetriever{res: rag.Result{
		Hits:  []rag.Fused{{SpanID: "span-that-did-not-travel", Path: "calc/calc.go", StartLine: 19, Score: 0.016, VectorScore: 0.42, VectorRank: 1, LexicalRank: 1}},
		Spans: map[string]models.Span{}, TopScore: 0.42, VectorRan: true, Mode: rag.ModeHybrid}}
	ae := emit(t, "ask-answered-empty.json", http.StatusOK,
		consolePost(emptyAssemble, "/api/repos/"+main.ID+"/ask", `{"q":"`+consoleQ+`"}`))
	mustContain(t, "ask-answered-empty.json", ae, `"citations":null`)
	mustContain(t, "ask-answered-empty.json", ae, `"answer":""`)
	mustContain(t, "ask-answered-empty.json", ae, `"refused":false`)

	// Class (b). unscored needs math.IsNaN(top) with VectorRan true, and the
	// real retriever sets TopScore to NaN only before the vector arm runs while
	// VectorRan is r.Mode != ModeLexical (retrieve.go:92) — so the pair is
	// unproducible from a query.
	unscored := consoleHandler(t, st, st, rag.ModeHybrid, rag.DefaultFloor(), 40)
	unscored.Rag = &fakeRetriever{res: rag.Result{
		Hits:      []rag.Fused{{SpanID: "span-a", Path: "calc/calc.go", StartLine: 19, Score: 0.016}},
		Spans:     map[string]models.Span{},
		TopScore:  nan(),
		VectorRan: true, Mode: rag.ModeHybrid}}
	un := emit(t, "ask-refused-unscored.json", http.StatusOK,
		consolePost(unscored, "/api/repos/"+main.ID+"/ask", `{"q":"`+consoleQ+`"}`))
	mustContain(t, "ask-refused-unscored.json", un, `"reason":"unscored"`)
	mustContain(t, "ask-refused-unscored.json", un, `"top_score":null`)

	emit(t, "symbols-one.json", http.StatusOK,
		consoleGet(hybrid, "/api/repos/"+main.ID+"/symbols?name=Total"))
	emit(t, "symbol.json", http.StatusOK,
		consoleGet(hybrid, "/api/repos/"+main.ID+"/symbols/"+push.ID))
	emit(t, "callers-approx-empty.json", http.StatusOK,
		consoleGet(hybrid, "/api/repos/"+main.ID+"/symbols/"+push.ID+"/callers"))

	// Class (b): two definitions of one name. The committed tree declares no
	// identifier twice, so the list fixture rule 5 requires cannot be emitted
	// from it; the rows are real declarations and the shaping is symbolOf,
	// matchedOf, clip and rag.NewStaleness.
	twoDefs := &stubReader{Reader: st, defs: []models.Symbol{total, renamed(table, "Total")}}
	emit(t, "symbols.json", http.StatusOK,
		consoleGet(consoleHandler(t, twoDefs, st, rag.ModeHybrid, rag.DefaultFloor(), 40),
			"/api/repos/"+main.ID+"/symbols?name=Total&suffix=true"))

	if table.SpanID != "" {
		t.Fatalf("big.go:2 is covered by span %s, so this corpus cannot produce a definition with no span", table.SpanID)
	}
	emit(t, "symbol-nospan.json", http.StatusOK,
		consoleGet(hybrid, "/api/repos/"+main.ID+"/symbols/"+table.ID))

	// Class (b): a caller row whose citation is null and a symbol name in both
	// lists. Neither is producible from this corpus — it holds one resolved
	// call edge and no unresolved call to a name it also defines.
	report := symbolAt(t, st, main, "use.go", "Report", "fixture", models.KindFunc, 10, 13)
	rich := &stubReader{Reader: st,
		callers: []store.Caller{
			{Symbol: total, Depth: 1, CallPath: "calc/use.go", CallLine: 7},
			{Symbol: table, Depth: 2, CallPath: "big.go", CallLine: 6},
		},
		approx: []store.Approximate{
			{Symbol: total, ToName: "Push", CallPath: "calc/use.go", CallLine: 7},
			{Symbol: report, ToName: "Push", CallPath: "use.go", CallLine: 12},
		}}
	emit(t, "callers.json", http.StatusOK,
		consoleGet(consoleHandler(t, rich, st, rag.ModeHybrid, rag.DefaultFloor(), 40),
			"/api/repos/"+main.ID+"/symbols/"+push.ID+"/callers?depth=2"))

	// Class (b): the approximate query's own failure is reported in the block
	// and never returned, because the precise callers are what was asked for
	// (graph.go:274-287).
	broken := &stubReader{Reader: st,
		callers:   []store.Caller{{Symbol: total, Depth: 1, CallPath: "calc/use.go", CallLine: 7}},
		approxErr: errors.New("fixture: the approximate caller query failed")}
	af := emit(t, "callers-approx-failed.json", http.StatusOK,
		consoleGet(consoleHandler(t, broken, st, rag.ModeHybrid, rag.DefaultFloor(), 40),
			"/api/repos/"+main.ID+"/symbols/"+push.ID+"/callers"))
	mustContain(t, "callers-approx-failed.json", af, `"failed":true`)

	emit(t, "error-400-depth.json", http.StatusBadRequest,
		consoleGet(hybrid, "/api/repos/"+main.ID+"/symbols/"+push.ID+"/callers?depth=40"))

	// --- the corpus listing, last among the reads because every successful
	// read winds last_queried_at and this is the one response that reports it.
	// Server order is last_queried_at DESC and is deliberately neither
	// alphabetical by remote nor insertion order.
	pinRepoClock(t, st, gl.ID, consoleNow.Add(-40*day), consoleNow.Add(-1*time.Minute))
	pinRepoClock(t, st, main.ID, consoleNow.Add(-3*day), consoleNow.Add(-2*time.Minute))
	pinRepoClock(t, st, cb.ID, consoleNow.Add(-9*day), consoleNow.Add(-3*time.Minute))
	rp := emit(t, "repos.json", http.StatusOK, consoleGet(hybrid, "/api/repos?limit=3"))
	assertReposOrder(t, rp, glRemote, ghRemote, cbRemote)

	// --- the failure bodies
	emit(t, "error-404-repo.json", http.StatusNotFound,
		consoleGet(hybrid, "/api/repos/0123456789abcdef0123456789abcdef"))
	emit(t, "error-404-job.json", http.StatusNotFound,
		consoleGet(hybrid, "/api/jobs/0123456789abcdef0123456789abcdef"))

	// 410, not 404: it existed, and that is a different fact (spec §10).
	if n, err := st.Evict(ctx, 6, 10); err != nil || n != 1 {
		t.Fatalf("evict: %d rows, err %v", n, err)
	}
	emit(t, "error-410-repo.json", http.StatusGone, consoleGet(hybrid, "/api/repos/"+evicted.ID))

	broke := consoleHandler(t, st, st, rag.ModeHybrid, rag.DefaultFloor(), 40)
	broke.Jobs = stubEnqueuer{err: errors.New("fixture: the jobs table is unreachable")}
	e5 := emit(t, "error-500.json", http.StatusInternalServerError,
		consoleGet(broke, "/api/jobs/"+pending))
	mustContain(t, "error-500.json", e5, consoleRequestID)
}

// ------------------------------------------------------------------ fixtures'
// own assertions. A fixture that stopped holding the property it was added for
// is a fixture whose tests pass for the wrong reason, so each is checked here
// rather than trusted.

func mustContain(t *testing.T, name string, body []byte, want string) {
	t.Helper()
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("%s no longer contains %s:\n%s", name, want, body)
	}
}

// nan is math.NaN through a variable, so the compiler cannot fold the literal
// the way a constant expression would be rejected.
func nan() float64 { return math.NaN() }

func seedJob(t *testing.T, st *store.Store, remote, seed string) string {
	t.Helper()
	ctx := context.Background()
	j, err := jobs.New(st.Pool()).Enqueue(ctx, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	id := store.Digest(seed)[:32]
	if _, err := st.Pool().Exec(ctx, `UPDATE jobs SET id = $2 WHERE id = $1`, j.ID, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// symbolAt builds one symbol row for a real declaration, linked to its span by
// the containment rule the indexer uses.
func symbolAt(t *testing.T, st *store.Store, repo models.Repo, path, name, pkg string, kind models.SpanKind, start, end int) models.Symbol {
	t.Helper()
	return models.Symbol{
		ID: store.SymbolID(repo.ID, path, start, string(kind), name), RepoID: repo.ID,
		FileID: store.FileID(repo.ID, path), Path: path, Name: name, Pkg: pkg, Kind: kind,
		StartLine: start, EndLine: end, SpanID: spanContaining(t, st, repo.ID, path, start),
	}
}

// seedConsoleGraph writes the call graph the committed files actually hold:
// five declarations and the five call sites written in them. Hand-built for the
// reason graph_live_test.go's callGraph is — apps/indexer is the other side of
// the deployment boundary this suite does not cross — and every path, range and
// line below is read off the committed bytes.
func seedConsoleGraph(t *testing.T, st *store.Store, repo models.Repo) (push, total, table models.Symbol) {
	t.Helper()
	push = symbolAt(t, st, repo, "calc/calc.go", "Machine.Push", "calc", models.KindFunc, 19, 23)
	total = symbolAt(t, st, repo, "calc/use.go", "Total", "calc", models.KindFunc, 3, 10)
	render := symbolAt(t, st, repo, "calc/calc.go", "Render", "calc", models.KindFunc, 25, 28)
	count := symbolAt(t, st, repo, "use.go", "Count", "fixture", models.KindFunc, 5, 8)
	report := symbolAt(t, st, repo, "use.go", "Report", "fixture", models.KindFunc, 10, 13)
	// big.go's Table is a var declaration longer than CHUNK_MAX_DECL_LINES, so
	// the chunker sub-windows it. Whether that leaves the declaration's first
	// line uncovered — and the symbol therefore with no span at all, which is
	// the state graph.go:37-39 keeps a key for — is measured, not assumed: see
	// the assertion at the call site.
	table = symbolAt(t, st, repo, "big.go", "Table", "fixture", models.KindVar, 2, 208)

	edge := func(from models.Symbol, to string, toName, path string, name string) models.Edge {
		off, line := callSiteIn(t, path, name)
		prov := models.ProvenanceSyntactic
		if to != "" {
			prov = models.ProvenanceResolved
		}
		return models.Edge{
			ID: store.EdgeID(repo.ID, from.ID, path, off, toName), RepoID: repo.ID,
			FromSymbolID: from.ID, ToSymbolID: to, ToName: toName,
			Kind: models.EdgeCalls, Provenance: prov, Path: path, Line: line,
		}
	}
	edges := []models.Edge{
		edge(total, push.ID, "Push", "calc/use.go", "Push"),
		edge(total, render.ID, "Render", "calc/use.go", "Render"),
		edge(report, count.ID, "Count", "use.go", "Count"),
		// fmt.Sprintf resolves outside the corpus, so there is no symbol row to
		// point at and the edge is syntactic (models.go:95-99).
		edge(render, "", "Sprintf", "calc/calc.go", "Sprintf"),
		edge(report, "", "Sprintf", "use.go", "Sprintf"),
	}
	if err := st.PutGraph(context.Background(), repo.ID,
		[]models.Symbol{push, total, render, count, report, table}, edges); err != nil {
		t.Fatal(err)
	}
	return push, total, table
}

// renamed is how the two-definition list is built: the same real declaration
// under the name the query asks for. See NOTES.md — the committed tree declares
// no identifier twice.
func renamed(sy models.Symbol, name string) models.Symbol {
	sy.Name = name
	sy.ID = store.SymbolID(sy.RepoID, sy.Path, sy.StartLine, string(sy.Kind), name)
	return sy
}

// assertHybridScores is the property search-hybrid.json exists for: a hit the
// vector arm returned, with a real cosine similarity, beside a hit only the
// lexical arm returned, whose vector_rank is 0 and whose vector_score is
// therefore null (read.go:545-550). search-lexical.json cannot hold it — with
// VectorRan false every score is null and the contrast does not exist.
func assertHybridScores(t *testing.T, body []byte) {
	t.Helper()
	var out struct {
		TopScore *float64 `json:"top_score"`
		Hits     []struct {
			VectorScore *float64 `json:"vector_score"`
			VectorRank  int      `json:"vector_rank"`
			LexicalRank int      `json:"lexical_rank"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.TopScore == nil {
		t.Fatal("search-hybrid.json has a null top_score; the vector arm did not run")
	}
	var scored, unscored int
	for _, h := range out.Hits {
		if h.VectorRank == 0 && h.VectorScore == nil {
			unscored++
		}
		if h.VectorRank > 0 && h.VectorScore != nil {
			scored++
		}
	}
	if scored == 0 || unscored == 0 {
		t.Fatalf("search-hybrid.json needs a real vector_score beside a null one; "+
			"got %d scored and %d unscored of %d hits", scored, unscored, len(out.Hits))
	}
}

func assertLexicalScores(t *testing.T, body []byte) {
	t.Helper()
	var out struct {
		TopScore *float64 `json:"top_score"`
		Hits     []struct {
			VectorScore *float64 `json:"vector_score"`
			VectorRank  int      `json:"vector_rank"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.TopScore != nil {
		t.Fatalf("search-lexical.json has top_score %v; lexical-only has no cosine similarity", *out.TopScore)
	}
	if len(out.Hits) < 2 {
		t.Fatalf("search-lexical.json has %d hits; a one-row list makes ordering unobservable", len(out.Hits))
	}
	for i, h := range out.Hits {
		if h.VectorScore != nil || h.VectorRank != 0 {
			t.Fatalf("search-lexical.json hit %d has vector rank %d score %v", i, h.VectorRank, h.VectorScore)
		}
	}
}

// assertReposOrder pins that the listing's order is neither alphabetical by
// remote nor insertion order, so a console that sorts is visible in a test
// rather than in a screenshot.
func assertReposOrder(t *testing.T, body []byte, want ...string) {
	t.Helper()
	var out struct {
		Repos []struct {
			Remote string `json:"remote"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Repos) != len(want) {
		t.Fatalf("repos.json holds %d repos, want %d", len(out.Repos), len(want))
	}
	for i, w := range want {
		if out.Repos[i].Remote != w {
			t.Fatalf("repos.json[%d] is %s, want %s", i, out.Repos[i].Remote, w)
		}
	}
	sorted := append([]string(nil), want...)
	slices.Sort(sorted)
	if slices.Equal(sorted, want) {
		t.Fatal("repos.json is in alphabetical order, so a client that sorts is indistinguishable from one that does not")
	}
}
