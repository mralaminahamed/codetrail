//go:build live

package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/testdb"
)

// scratchDSN names a database this suite creates for itself, as every other
// live suite does: the eviction test below clears a whole corpus, and
// `go test -tags=live ./...` runs the packages at once.
var scratchDSN string

func TestMain(m *testing.M) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		// A skipped live suite prints the same "ok" as one that ran, so in CI a
		// dropped DATABASE_URL would look green with zero live coverage.
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, "DATABASE_URL unset in CI — the live suite must never silently skip")
			os.Exit(1)
		}
		os.Exit(m.Run()) // every test skips; see liveStore
	}
	// Spec §9: the mechanics are provable with no model. Here the fake is also
	// what makes the corpus deterministic — a query drawn from a span retrieves
	// that span because both are hashed bags of the same words. Defaulted
	// rather than demanded and refused rather than overridden, exactly as the
	// indexer's live suite does it, so neither a bare `go test -tags=live ./...`
	// nor a shell already pointing at Ollama can pick the wrong one silently.
	switch p := os.Getenv("EMBED_PROVIDER"); p {
	case "":
		os.Setenv("EMBED_PROVIDER", "fake")
	case "fake":
	default:
		fmt.Fprintf(os.Stderr, "EMBED_PROVIDER=%q: the live suite runs on the fake embedder\n", p)
		os.Exit(1)
	}
	code, err := testdb.Scratch(base, "codetrail_gateway", func(d string) int {
		scratchDSN = d
		return m.Run()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func liveStore(t *testing.T) *store.Store {
	t.Helper()
	if scratchDSN == "" {
		t.Skip("set DATABASE_URL to run")
	}
	st, err := store.New(context.Background(), scratchDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// fixtureRoot is the indexer's committed tree, indexed here rather than copied.
// One fixture, so the corpus this suite retrieves over is the corpus
// apps/indexer/cmd/index_live_test.go already measured span by span; a second
// copy would be a second thing to keep in agreement, and the doc.go asymmetry
// pinned below is a property of exactly these files.
//
// The ".txt" suffix comes off the path: walk classifies by extension and
// ".go.txt" is no language at all. It is on disk because `gofmt -l apps` walks
// testdata and would report the deliberately unparseable fixture.
const fixtureRoot = "../../../../apps/indexer/cmd/testdata/repo"

// langByExt is the extension map walk.Files applies, narrowed to what this
// fixture holds. Copied rather than imported: walk lives under
// apps/indexer/internal, which the gateway may not import — that confinement
// is the deployment boundary P1 built, and the indexer's own live suite is
// what covers the walk. What this suite needs from the indexer path is the
// part that decides a span's identity, and that is chunk, embed and store,
// which are shared.
var langByExt = map[string]string{".go": "go", ".md": "markdown", ".yaml": "yaml"}

// The paths that produce spans under each strategy, which is the asymmetry P2
// measured: doc.go holds only a package comment and tools.go only imports, and
// f.Doc is not a Decl, so the AST arm emits nothing for either.
var (
	astPaths    = []string{"README.md", "big.go", "calc/broken.go", "calc/calc.go", "calc/use.go", "config.yaml", "use.go"}
	windowPaths = []string{"README.md", "big.go", "calc/broken.go", "calc/calc.go", "calc/use.go", "config.yaml", "doc.go", "tools.go", "use.go"}
)

// fixtureBytes is the file a citation names, as committed. The byte-for-byte
// check below reads this and nothing the indexing path produced, which is what
// makes it a check rather than a restatement.
func fixtureBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, filepath.FromSlash(path)+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// indexFixture writes the fixture tree into one repository through the shared
// packages the indexer writes with — chunk, the embedder from the environment,
// and store — and returns the repo row the read path serves.
//
// The embedder comes from embed.FromEnv, the constructor both binaries use,
// rather than being built by hand: an embedder wired here would prove the
// pipeline works with one production never builds.
func indexFixture(t *testing.T, st *store.Store, name string, strategy chunk.Strategy) models.Repo {
	t.Helper()
	ctx := context.Background()

	emb, err := embed.FromEnv(ctx, store.EmbeddingDim, store.CheckDim, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opt := chunk.Defaults()
	opt.Strategy = strategy

	remote := "https://github.com/codetrail-live/" + name
	// A commit that looks like one, fixed per repository, so a citation's
	// permalink and its repo id are both stable across runs.
	commit := store.Digest(name)[:40]
	repo := models.Repo{ID: store.RepoID(remote, commit), Remote: remote, Ref: "main",
		Commit: commit, SizeBytes: 4096, IndexedAt: time.Now().Add(-72 * time.Hour)}

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
	if len(vecs) != len(spans) {
		t.Fatalf("%s returned %d vectors for %d spans", emb.Model(), len(vecs), len(spans))
	}
	for i := range vecs {
		spans[i].Embedding = vecs[i]
	}
	if err := st.PutRepo(ctx, repo, files); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSpans(ctx, repo.ID, spans, emb.Model(), emb.Dim()); err != nil {
		t.Fatal(err)
	}

	// The corpus this suite retrieves over is asserted, not assumed: the
	// asymmetry the tests below turn on is a property of which files produced
	// spans, and a fixture that quietly stopped holding it would leave those
	// tests passing for the wrong reason.
	var covered []string
	for _, sp := range spans {
		if !slices.Contains(covered, sp.Path) {
			covered = append(covered, sp.Path)
		}
	}
	slices.Sort(covered)
	want := astPaths
	if strategy == chunk.StrategyWindow {
		want = windowPaths
	}
	if !slices.Equal(covered, want) {
		t.Fatalf("the %s corpus covers %v, want %v", strategy, covered, want)
	}
	return repo
}

// liveHandler serves the read routes over a real store and a real Retriever,
// with the knobs main boots with (apps/gateway/cmd/main_test.go pins those
// defaults against newRetriever itself; here they are what production runs, so
// the composition under test is the shipped one).
func liveHandler(t *testing.T, st *store.Store, mode rag.Mode, floor rag.Floor) *Handler {
	t.Helper()
	emb, err := embed.FromEnv(context.Background(), store.EmbeddingDim, store.CheckDim, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{
		Repos: st,
		Rag: &rag.Retriever{Store: st, Emb: emb, Mode: mode,
			K: 60, Candidates: 40, Split: true, Floor: floor},
		Floor: floor, Budget: rag.DefaultBudget(), Now: time.Now,
	}
}

func post(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(echoContentType, echoJSON)
	rec := httptest.NewRecorder()
	mount(h).ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mount(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

const (
	echoContentType = "Content-Type"
	echoJSON        = "application/json"
)

// askResult is the answer payload, decoded into the fields the assertions
// below check rather than into a map: a citation is a tuple, and reading it
// field by field out of map[string]any is where an assertion stops naming what
// it compares.
type askResult struct {
	RepoID     string `json:"repo_id"`
	Refused    bool   `json:"refused"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail"`
	AnsweredBy string `json:"answered_by"`
	Answer     string `json:"answer"`
	Mode       string `json:"mode"`
	TopScore   *float64
	Citations  []struct {
		Marker   int    `json:"marker"`
		SpanID   string `json:"span_id"`
		Symbol   string `json:"symbol"`
		Citation struct {
			RepoID    string `json:"repo_id"`
			Commit    string `json:"commit"`
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
			Digest    string `json:"digest"`
			Permalink string `json:"permalink"`
			Staleness struct {
				State        string `json:"state"`
				ForgeChecked bool   `json:"forge_checked"`
				Note         string `json:"note"`
			} `json:"staleness"`
		} `json:"citation"`
	} `json:"citations"`
	Floor struct {
		Value      float64 `json:"value"`
		Calibrated bool    `json:"calibrated"`
		Applicable bool    `json:"applicable"`
	} `json:"floor"`
}

func ask(t *testing.T, h *Handler, repoID, q string) (askResult, *httptest.ResponseRecorder) {
	t.Helper()
	rec := post(t, h, "/api/repos/"+repoID+"/ask", `{"q":`+quote(q)+`}`)
	var out askResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	// top_score is null for a lexical-only or empty result, so it is read
	// separately rather than through a float64 that would decode null as 0.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err == nil {
		if v, ok := raw["top_score"]; ok && string(v) != "null" {
			var f float64
			if err := json.Unmarshal(v, &f); err == nil {
				out.TopScore = &f
			}
		}
	}
	return out, rec
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// The phase's headline claim, end to end and on a live database: a question
// gets an answer, and every citation in it is checkable — the digest hashes the
// text the answer showed, and the line range brackets those bytes in the file
// at the cited commit.
//
// The text is read back out of the assembled answer rather than out of the
// store, because the claim is about what a caller was shown. An assembler that
// truncated a span would leave the store's row untouched and the answer's text
// unhashable, which is exactly the citation that lies.
//
// A second repository holds a strictly better match for this query — the window
// corpus indexes doc.go and calc.go's prose whole — so a query that lost its
// repo filter would cite it.
func TestAskOverARealIndexReturnsACitationWhoseDigestMatchesTheSpan(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "ask-ast", chunk.StrategyAST)
	other := indexFixture(t, st, "ask-window", chunk.StrategyWindow)

	res, rec := ask(t, liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor()), repo.ID,
		"how does the machine push a running total")
	if rec.Code != http.StatusOK || res.Refused {
		t.Fatalf("want an answer, got %d: %s", rec.Code, rec.Body)
	}
	if len(res.Citations) == 0 {
		t.Fatal("an answer with no citations is not an extractive answer")
	}
	if res.AnsweredBy != "extractive" {
		t.Errorf("answered_by %q, want extractive", res.AnsweredBy)
	}
	if res.TopScore == nil {
		t.Fatal("hybrid over a scored corpus reported no top score")
	}

	blocks := blocksOf(t, res.Answer, res)
	for i, c := range res.Citations {
		cit := c.Citation
		// The span itself, read back under the asked repository. Not
		// citation.repo_id: NewCitation fills that from the repository the
		// route named, so a span that leaked in from another one would still
		// carry this id — measured, by defeating the repo filter in both arms
		// and watching a foreign span arrive with the right repo_id on it.
		// GetSpan is repo-scoped, so a leaked span is ErrNotFound here.
		stored, err := st.GetSpan(context.Background(), repo.ID, c.SpanID)
		if err != nil {
			t.Fatalf("citation %d cites span %s, which is not in %s: %v",
				c.Marker, c.SpanID, repo.ID, err)
		}
		if stored.Digest != cit.Digest || stored.Path != cit.Path {
			t.Errorf("citation %d says %s@%s, the stored row says %s@%s",
				c.Marker, cit.Path, cit.Digest, stored.Path, stored.Digest)
		}
		if cit.Commit != repo.Commit {
			t.Errorf("citation %d cites commit %s, want %s", c.Marker, cit.Commit, repo.Commit)
		}
		text := blocks[i]
		sum := sha256.Sum256([]byte(text))
		if got := hex.EncodeToString(sum[:]); got != cit.Digest {
			t.Errorf("citation %d (%s:%d-%d): the answer's text hashes to %s, the citation claims %s",
				c.Marker, cit.Path, cit.StartLine, cit.EndLine, got, cit.Digest)
		}
		// The other half, and the one a digest cannot make: that the range
		// names those bytes in the file at that commit. A chunker that shifted
		// the range and the text together is self-consistent and still wrong.
		if !bracketsExactly(fixtureBytes(t, cit.Path), text, cit.StartLine, cit.EndLine) {
			t.Errorf("citation %d: lines %d-%d of %s are not the bytes the answer shows:\n%s",
				c.Marker, cit.StartLine, cit.EndLine, cit.Path, text)
		}
		if want := "https://github.com/codetrail-live/ask-ast/blob/" + repo.Commit + "/" +
			cit.Path + fmt.Sprintf("#L%d-L%d", cit.StartLine, cit.EndLine); cit.Permalink != want {
			t.Errorf("citation %d permalink %q, want %q", c.Marker, cit.Permalink, want)
		}
		// The staleness claim is the one the corpus can prove and no more.
		if cit.Staleness.ForgeChecked || cit.Staleness.State != "unknown" {
			t.Errorf("citation %d claims %+v; codetrail never asked the forge", c.Marker, cit.Staleness)
		}
		if !strings.Contains(cit.Staleness.Note, "has not checked whether") {
			t.Errorf("citation %d note does not say what was not checked: %q", c.Marker, cit.Staleness.Note)
		}
	}
	// The other corpus is reachable and holds the parallel spans, so the scope
	// assertion above is a filter doing work rather than a repository nobody
	// could have cited.
	if res2, rec2 := ask(t, liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor()), other.ID,
		"how does the machine push a running total"); rec2.Code != http.StatusOK || len(res2.Citations) == 0 {
		t.Fatalf("the second repository answered nothing, so nothing could have leaked from it: %s", rec2.Body)
	}
}

// blocksOf recovers the text shown under each citation marker.
//
// By locating each citation's own header rather than by splitting on the
// separator between blocks: a span's text can itself contain a blank line
// followed by a bracket, and a split would then cut a span in half and the
// digest check would fail for the wrong reason.
func blocksOf(t *testing.T, answer string, res askResult) []string {
	t.Helper()
	heads := make([]string, len(res.Citations))
	for i, c := range res.Citations {
		cit := c.Citation
		h := fmt.Sprintf("[%d] %s:%d-%d", c.Marker, cit.Path, cit.StartLine, cit.EndLine)
		if c.Symbol != "" {
			h += " (" + c.Symbol + ")"
		}
		heads[i] = h + "\n"
	}
	out := make([]string, len(heads))
	for i, h := range heads {
		at := strings.Index(answer, h)
		if at < 0 {
			t.Fatalf("the answer has no block for citation %d (%q):\n%s", i+1, h, answer)
		}
		body := answer[at+len(h):]
		if i+1 < len(heads) {
			next := strings.Index(body, "\n\n"+heads[i+1])
			if next < 0 {
				t.Fatalf("citation %d's block does not end at citation %d's header:\n%s", i+1, i+2, answer)
			}
			body = body[:next]
		}
		out[i] = body
	}
	return out
}

// bracketsExactly reports whether text occupies exactly lines start..end of
// body, 1-based and inclusive. The indexer's live suite carries the same
// function for the same reason: re-running the chunker's own line arithmetic
// here would agree with it about an off-by-one, so this counts newlines around
// a byte offset instead.
func bracketsExactly(body []byte, text string, start, end int) bool {
	if text == "" {
		return false
	}
	t := []byte(text)
	for off := 0; off+len(t) <= len(body); {
		i := indexAt(body[off:], t)
		if i < 0 {
			return false
		}
		at := off + i
		startsLine := at == 0 || body[at-1] == '\n'
		endsLine := at+len(t) == len(body) || body[at+len(t)] == '\n'
		if startsLine && endsLine &&
			countNL(body[:at]) == start-1 && countNL(t) == end-start {
			return true
		}
		off = at + 1
	}
	return false
}

func indexAt(hay, needle []byte) int { return strings.Index(string(hay), string(needle)) }
func countNL(b []byte) int           { return strings.Count(string(b), "\n") }

// The floor is a live mechanism, proved without asserting anything about its
// value: one query, one corpus, two floors, two outcomes. Spec:315 puts the
// number in P6, so what is pinned here is that the knob decides and that a
// refusal is a 200 carrying a reason from the closed set.
func TestAHighFloorRefusesTheSameQueryTheDefaultAnswers(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "floor", chunk.StrategyAST)
	const q = "how does the machine push a running total"

	answered, rec := ask(t, liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor()), repo.ID, q)
	if rec.Code != http.StatusOK || answered.Refused {
		t.Fatalf("the shipped default refused: %d %s", rec.Code, rec.Body)
	}
	if answered.Floor.Value != -1 || answered.Floor.Calibrated {
		t.Errorf("floor %+v on the wire, want -1 uncalibrated", answered.Floor)
	}
	if !answered.Floor.Applicable {
		t.Error("the floor reports itself inapplicable in hybrid mode, where there is a cosine similarity")
	}
	if answered.TopScore == nil {
		t.Fatal("no top score, so the pair below would not be a floor comparison")
	}
	// A floor of 1 refuses only because this query is not a copy of any span.
	// Asserted rather than assumed: a fixture whose top score were exactly 1
	// would answer under both floors and the pair would prove nothing.
	if *answered.TopScore >= 1 {
		t.Fatalf("top score %v: the query matches a span exactly, so a floor of 1 cannot separate the two runs",
			*answered.TopScore)
	}

	before := counters(t)
	refused, rec := ask(t, liveHandler(t, st, rag.ModeHybrid, rag.Floor{Value: 1}), repo.ID, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("a refusal is an outcome, not an error: %d %s", rec.Code, rec.Body)
	}
	if !refused.Refused || refused.Reason != string(rag.ReasonBelowFloor) {
		t.Fatalf("want refused=true below_floor, got %+v", refused)
	}
	if refused.Floor.Value != 1 || refused.Floor.Calibrated {
		t.Errorf("the refusal reports floor %+v; an operator's own number is still not measured", refused.Floor)
	}
	if !strings.Contains(refused.Detail, "not calibrated") {
		t.Errorf("the refusal detail does not say the floor is uncalibrated: %q", refused.Detail)
	}
	// The whole set of series that moved, not only the one expected: a mutant
	// that incremented both counters passes an assertion naming one.
	want := map[string]float64{
		`codetrail_answer_total{outcome="refused"}`:     1,
		`codetrail_refusal_total{reason="below_floor"}`: 1,
	}
	if moved := movedSince(t, before); !maps.Equal(moved, want) {
		t.Errorf("counters moved %v, want %v", moved, want)
	}
}

// Spec §10's three outcomes kept apart on a live database, since the hermetic
// pair proves the handler and not the path a real request takes.
//
// The error is a real one: a corpus indexed at 768 components, queried by an
// embedder of a different width. The store answers, the widths disagree, and
// that is a server-side failure rather than "we had nothing to say".
func TestARefusalAnErrorAndAValidationFailureAreThreeOutcomesLive(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "outcomes", chunk.StrategyAST)

	h := liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor())
	// A narrower embedder than the corpus was written with. rag refuses before
	// embedding anything, so this is the mismatch and not a pgvector error.
	h.Rag = &rag.Retriever{Store: st, Emb: embed.NewFake(16), Mode: rag.ModeHybrid,
		K: 60, Candidates: 40, Split: true, Floor: rag.DefaultFloor()}

	before := counters(t)
	rec := post(t, h, "/api/repos/"+repo.ID+"/ask", `{"q":"how does the machine push a running total"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a corpus in another vector space is an error, got %d: %s", rec.Code, rec.Body)
	}
	got := body(t, rec)
	if got["error"] != "internal error" {
		t.Errorf("the 500 body carries more than an opaque error: %s", rec.Body)
	}
	if rid, _ := got["request_id"].(string); rid == "" {
		t.Errorf("the 500 carries no request id, so a caller cannot quote one: %s", rec.Body)
	}
	if moved, want := movedSince(t, before),
		map[string]float64{`codetrail_answer_total{outcome="error"}`: 1}; !maps.Equal(moved, want) {
		t.Errorf("counters moved %v, want %v", moved, want)
	}

	// A rejected request never reached the answerer, so it is no outcome at
	// all: counting it would inflate the rate §10 wants readable.
	before = counters(t)
	rec = post(t, liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor()),
		"/api/repos/"+repo.ID+"/ask", `{"q":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an empty question is a 400, got %d: %s", rec.Code, rec.Body)
	}
	if moved := movedSince(t, before); len(moved) != 0 {
		t.Errorf("a validation failure moved %v; it must move no outcome counter at all", moved)
	}
}

// emptyRepo is an indexed repository whose index holds no spans, which is what
// the indexer leaves behind for a tree of unsupported extensions and for an AST
// strategy over files that declare nothing. PutRepo without PutSpans is that
// row exactly.
func emptyRepo(t *testing.T, st *store.Store, name string) models.Repo {
	t.Helper()
	ctx := context.Background()
	remote := "https://github.com/codetrail-live/" + name
	commit := store.Digest(name)[:40]
	repo := models.Repo{ID: store.RepoID(remote, commit), Remote: remote, Ref: "main",
		Commit: commit, SizeBytes: 4096, IndexedAt: time.Now().Add(-72 * time.Hour)}
	// One file, of a language walk does not classify: the repository was
	// indexed and holds content, and still no span was written for it.
	f := models.File{ID: store.FileID(repo.ID, "LICENSE"), RepoID: repo.ID, Path: "LICENSE",
		Blob: store.Digest("license")[:40], Lines: 3}
	if err := st.PutRepo(ctx, repo, []models.File{f}); err != nil {
		t.Fatal(err)
	}
	// Asserted rather than assumed: the whole test below is about a corpus with
	// no spans, and one that quietly had some would pass for the wrong reason.
	n, err := st.CountSpans(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the empty corpus holds %d spans", n)
	}
	return repo
}

// Spec §10 at the outcome most likely to reach a real user: a repository that
// was indexed and produced no spans. The corpus has nothing to rank, which is a
// refusal — and until this was fixed the two modes that run the vector arm
// answered 500, so the error counter moved and no_spans was unreachable at the
// shipped default.
//
// All three modes, because the defect was in the vector arm's own precondition:
// lexical mode reached the refusal already and is the control that shows the
// other two now agree with it.
func TestARepoIndexedWithNoSpansRefusesInEveryMode(t *testing.T) {
	st := liveStore(t)
	repo := emptyRepo(t, st, "no-spans")

	for _, mode := range []rag.Mode{rag.ModeHybrid, rag.ModeVector, rag.ModeLexical} {
		t.Run(string(mode), func(t *testing.T) {
			h := liveHandler(t, st, mode, rag.DefaultFloor())
			before := counters(t)
			res, rec := ask(t, h, repo.ID, "how does the machine push a running total")
			moved := movedSince(t, before)

			if rec.Code != http.StatusOK {
				t.Fatalf("%s: an empty corpus is a refusal, got %d: %s", mode, rec.Code, rec.Body)
			}
			// The reason, not merely the refusal: no_spans and below_floor are
			// different facts about this repository.
			if !res.Refused || res.Reason != string(rag.ReasonNoSpans) {
				t.Fatalf("%s: refused=%v reason=%q, want a no_spans refusal: %s",
					mode, res.Refused, res.Reason, rec.Body)
			}
			if res.Mode != string(mode) || res.TopScore != nil {
				t.Errorf("%s: mode %q top_score %v", mode, res.Mode, res.TopScore)
			}
			want := map[string]float64{
				`codetrail_answer_total{outcome="refused"}`:  1,
				`codetrail_refusal_total{reason="no_spans"}`: 1,
			}
			if !maps.Equal(moved, want) {
				t.Errorf("%s: counters moved %v, want %v — a refusal must not move the error counter",
					mode, moved, want)
			}

			// Search ranks rather than refuses, so the same corpus is an empty
			// list there, and a 200: nothing to rank is not a failure.
			rec = post(t, h, "/api/repos/"+repo.ID+"/search", `{"q":"machine"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: search over an empty corpus: %d %s", mode, rec.Code, rec.Body)
			}
			out := body(t, rec)
			if out["count"] != float64(0) || out["mode"] != string(mode) {
				t.Errorf("%s: search body %s", mode, rec.Body)
			}
		})
	}
}

// repoRoutesFor is every route keyed on a repo id, which is the set the
// 404/410 distinction has to hold across rather than on the one route a test
// happened to drive.
func repoRoutesFor(id, spanID, symbolID string) []struct {
	method, path, body string
} {
	return []struct{ method, path, body string }{
		{http.MethodGet, "/api/repos/" + id, ""},
		{http.MethodGet, "/api/repos/" + id + "/spans/" + spanID, ""},
		{http.MethodPost, "/api/repos/" + id + "/search", `{"q":"machine"}`},
		{http.MethodPost, "/api/repos/" + id + "/ask", `{"q":"machine"}`},
		{http.MethodGet, "/api/repos/" + id + "/symbols?name=Add", ""},
		{http.MethodGet, "/api/repos/" + id + "/symbols/" + symbolID, ""},
		{http.MethodGet, "/api/repos/" + id + "/symbols/" + symbolID + "/callers", ""},
	}
}

// liveGraph writes a small call graph over an indexed fixture repository: three
// definitions and the two edges between them, one resolved and one syntactic.
//
// Hand-built rather than produced by the indexer's graph stage, for the reason
// this whole suite copies langByExt rather than importing walk — apps/indexer
// is the other side of the deployment boundary. What it needs from the indexer
// path is what decides a row's identity, and SymbolID and EdgeID are shared.
//
// The definitions link to real spans, so a citation's digest is the hash of
// text on disk; Other deliberately links to none, which is Task 2's
// sub-windowed declaration and the only row that can tell a null citation from
// a rendered guess.
func liveGraph(t *testing.T, st *store.Store, repo models.Repo) (add, other models.Symbol) {
	t.Helper()
	ctx := context.Background()
	span := func(path string) (string, int, int) {
		t.Helper()
		var id string
		var start, end int
		if err := st.Pool().QueryRow(ctx, `
			SELECT id, start_line, end_line FROM spans
			WHERE repo_id = $1 AND path = $2 ORDER BY start_line LIMIT 1`,
			repo.ID, path).Scan(&id, &start, &end); err != nil {
			t.Fatal(err)
		}
		return id, start, end
	}
	sym := func(path, name string, start, end int, spanID string) models.Symbol {
		return models.Symbol{
			ID:     store.SymbolID(repo.ID, path, start, string(models.KindFunc), name),
			RepoID: repo.ID, FileID: store.FileID(repo.ID, path), Path: path,
			Name: name, Pkg: "calc", Kind: models.KindFunc,
			StartLine: start, EndLine: end, SpanID: spanID,
		}
	}
	calcSpan, calcStart, calcEnd := span("calc/calc.go")
	useSpan, useStart, useEnd := span("calc/use.go")
	add = sym("calc/calc.go", "Add", calcStart, calcEnd, calcSpan)
	use := sym("calc/use.go", "Use", useStart, useEnd, useSpan)
	other = sym("calc/use.go", "Other", useEnd+1, useEnd+40, "")

	edge := func(from models.Symbol, off, line int, toName, to string) models.Edge {
		prov := models.ProvenanceSyntactic
		if to != "" {
			prov = models.ProvenanceResolved
		}
		return models.Edge{
			ID: store.EdgeID(repo.ID, from.ID, from.Path, off, toName), RepoID: repo.ID,
			FromSymbolID: from.ID, ToSymbolID: to, ToName: toName,
			Kind: models.EdgeCalls, Provenance: prov, Path: from.Path, Line: line,
		}
	}
	if err := st.PutGraph(ctx, repo.ID,
		[]models.Symbol{add, use, other},
		[]models.Edge{
			edge(use, 100, useStart+1, "Add", add.ID),
			// The same name, unresolved: the two answers must not merge.
			edge(other, 200, useEnd+2, "Add", ""),
		}); err != nil {
		t.Fatal(err)
	}
	return add, other
}

// Spec §10 on a live database, in both directions: an indexed repository never
// answers 410, and one that was indexed and evicted never answers 404. The
// second is what the tombstone exists for — eviction is one DELETE that
// cascades, and without a row saying so nothing distinguishes an evicted
// repository from one that was never submitted.
func TestAnEvictedRepoAnswersGoneOnEveryReadRoute(t *testing.T) {
	ctx := context.Background()
	st := liveStore(t)
	repo := indexFixture(t, st, "evicted", chunk.StrategyAST)
	h := liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor())

	// One real span id, so the span route is exercised on a row that exists
	// before eviction and is gone after it.
	var spanID string
	if err := st.Pool().QueryRow(ctx,
		`SELECT id FROM spans WHERE repo_id = $1 ORDER BY path, start_line LIMIT 1`, repo.ID).
		Scan(&spanID); err != nil {
		t.Fatal(err)
	}

	add, _ := liveGraph(t, st, repo)
	for _, r := range repoRoutesFor(repo.ID, spanID, add.ID) {
		rec := drive(t, h, r.method, r.path, r.body)
		if rec.Code == http.StatusGone {
			t.Errorf("%s %s: a live repository answered 410", r.method, r.path)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: want 200 before eviction, got %d: %s", r.method, r.path, rec.Code, rec.Body)
		}
	}

	if _, err := st.Evict(ctx, 0, 100); err != nil {
		t.Fatal(err)
	}
	for _, r := range repoRoutesFor(repo.ID, spanID, add.ID) {
		rec := drive(t, h, r.method, r.path, r.body)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s %s: an evicted repository answered 404, which forgets it was ever here",
				r.method, r.path)
		}
		if rec.Code != http.StatusGone {
			t.Errorf("%s %s: want 410 after eviction, got %d: %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	// An id that was never submitted is still a 404, or the 410 above would be
	// what this handler answers for everything it cannot find.
	if rec := get(t, h, "/api/repos/"+store.RepoID("https://github.com/codetrail-live/never", "0")); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown repository answered %d, want 404: %s", rec.Code, rec.Body)
	}
}

func drive(t *testing.T, h *Handler, method, path, reqBody string) *httptest.ResponseRecorder {
	t.Helper()
	if method == http.MethodGet {
		return get(t, h, path)
	}
	return post(t, h, path, reqBody)
}

// The asymmetry P2 measured, pinned rather than papered over: a file holding
// only a package comment produces no AST spans, so under the production
// strategy its prose is unretrievable. The same corpus under
// CHUNK_STRATEGY=window answers from it.
//
// Corrected against measurement. The plan expected the AST corpus to *refuse*
// this question. It does not, and the reason is worth having in the test list:
// the vector arm returns its top candidates whatever they score, so a hybrid
// ask over the AST corpus answers from unrelated spans instead. The refusal is
// real in lexical mode, where a term no span holds retrieves nothing. Both are
// asserted, because "answers from the wrong file" is the outcome an operator
// actually gets and it is a worse failure to leave undocumented than a refusal.
func TestPackageDocProseIsUnretrievableUnderTheASTStrategy(t *testing.T) {
	st := liveStore(t)
	ast := indexFixture(t, st, "doc-ast", chunk.StrategyAST)
	win := indexFixture(t, st, "doc-window", chunk.StrategyWindow)
	// Three words that occur nowhere in this fixture but doc.go's package
	// comment, so the lexical arm's OR cannot reach a span through any of them.
	const q = "documents itself declares"

	hybrid := liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor())
	res, rec := ask(t, hybrid, ast.ID, q)
	if rec.Code != http.StatusOK || res.Refused {
		t.Fatalf("hybrid over the AST corpus: %d %s", rec.Code, rec.Body)
	}
	for _, c := range res.Citations {
		if c.Citation.Path == "doc.go" {
			t.Errorf("the AST corpus cited doc.go at %d-%d; it has no spans there",
				c.Citation.StartLine, c.Citation.EndLine)
		}
	}
	if strings.Contains(res.Answer, "documents itself") {
		t.Error("the AST corpus answered with doc.go's prose, which it does not index")
	}

	// The same question, the same bytes, the other strategy: answered, from the
	// file the AST arm cannot see.
	winRes, rec := ask(t, hybrid, win.ID, q)
	if rec.Code != http.StatusOK || winRes.Refused {
		t.Fatalf("hybrid over the window corpus: %d %s", rec.Code, rec.Body)
	}
	var citedDoc bool
	for _, c := range winRes.Citations {
		citedDoc = citedDoc || c.Citation.Path == "doc.go"
	}
	if !citedDoc {
		t.Fatalf("the window corpus did not cite doc.go either, so the asymmetry above proves nothing: %+v",
			winRes.Citations)
	}

	// Lexical only, where a term nothing holds retrieves nothing: the AST
	// corpus refuses and the window corpus answers.
	lex := liveHandler(t, st, rag.ModeLexical, rag.DefaultFloor())
	lexAST, rec := ask(t, lex, ast.ID, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("a refusal is a 200: %d %s", rec.Code, rec.Body)
	}
	if !lexAST.Refused || lexAST.Reason != string(rag.ReasonNoSpans) {
		t.Fatalf("lexical over the AST corpus: want refused no_spans, got %+v", lexAST)
	}
	// No cosine similarity in this mode, so the floor is not a thing that ran.
	if lexAST.Floor.Applicable {
		t.Error("the floor claims to apply in lexical mode, where ts_rank_cd is the only score")
	}
	lexWin, rec := ask(t, lex, win.ID, q)
	if rec.Code != http.StatusOK || lexWin.Refused {
		t.Fatalf("lexical over the window corpus: want an answer, got %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(lexWin.Answer, "documents itself") {
		t.Errorf("the window corpus answered without doc.go's prose:\n%s", lexWin.Answer)
	}
}

// Both arms run against a real corpus and each returns a ranked list with the
// per-arm ranks a reader needs to see an arm's contribution (spec:316).
//
// Wiring only, and it says so: CI runs on embed.Fake, a hashed bag of words
// over the same span text the lexical arm indexes, so the two arms are near
// duplicates of one function here. Nothing in this test compares the modes,
// and nothing may: which retrieves better is P6's experiment.
func TestEveryRetrievalModeRetrievesFromALiveCorpus(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "modes", chunk.StrategyAST)

	for _, mode := range []rag.Mode{rag.ModeVector, rag.ModeLexical, rag.ModeHybrid} {
		h := liveHandler(t, st, mode, rag.DefaultFloor())
		rec := post(t, h, "/api/repos/"+repo.ID+"/search", `{"q":"machine push running total"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", mode, rec.Code, rec.Body)
		}
		out := body(t, rec)
		if out["mode"] != string(mode) {
			t.Errorf("%s: the response says mode %v", mode, out["mode"])
		}
		hits, _ := out["hits"].([]any)
		if len(hits) == 0 {
			t.Fatalf("%s retrieved nothing from a corpus holding the query's own words", mode)
		}
		var vector, lexical int
		for _, h := range hits {
			m, _ := h.(map[string]any)
			if v, _ := m["vector_rank"].(float64); v > 0 {
				vector++
			}
			if v, _ := m["lexical_rank"].(float64); v > 0 {
				lexical++
			}
		}
		switch mode {
		case rag.ModeVector:
			if vector != len(hits) || lexical != 0 {
				t.Errorf("vector mode: %d/%d ranked by the vector arm, %d by the lexical one",
					vector, len(hits), lexical)
			}
			if out["top_score"] == nil {
				t.Error("vector mode reported no top score")
			}
		case rag.ModeLexical:
			if lexical != len(hits) || vector != 0 {
				t.Errorf("lexical mode: %d/%d ranked by the lexical arm, %d by the vector one",
					lexical, len(hits), vector)
			}
			// No cosine similarity exists in this mode, and null is the honest
			// encoding of a number there is none of.
			if out["top_score"] != nil {
				t.Errorf("lexical mode reported a top score of %v", out["top_score"])
			}
		case rag.ModeHybrid:
			if vector == 0 || lexical == 0 {
				t.Errorf("hybrid returned %d vector-ranked and %d lexical-ranked hits; one arm did not run",
					vector, lexical)
			}
		}
	}
}

// The repo view's graph counts, over a graph a query actually holds. Three
// separate numbers rather than a ratio, and the provenance split from its own
// two predicates: a repository whose edges all resolve makes the two counts
// equal, and a view that read one total twice would look right.
func TestTheRepoViewCountsTheGraphSplitByProvenance(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "graphcounts", chunk.StrategyAST)
	h := liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor())

	// Before the graph exists the counts are zeros, not absent keys: a client
	// cannot otherwise tell "no graph" from "this build does not report one".
	empty := body(t, get(t, h, "/api/repos/"+repo.ID))
	for _, k := range []string{"symbols", "edges", "edges_resolved", "edges_syntactic"} {
		if v, ok := empty[k]; !ok || v != float64(0) {
			t.Errorf("before the graph, %s = %v (present %v)", k, v, ok)
		}
	}

	liveGraph(t, st, repo)
	out := body(t, get(t, h, "/api/repos/"+repo.ID))
	for k, want := range map[string]float64{
		"symbols": 3, "edges": 2, "edges_resolved": 1, "edges_syntactic": 1,
	} {
		if out[k] != want {
			t.Errorf("%s = %v, want %v", k, out[k], want)
		}
	}
	// The span counts are still the chunker's, so the graph did not overwrite
	// what the view already said.
	if out["files"] == float64(0) || out["spans"] == float64(0) {
		t.Errorf("the graph counts displaced the corpus counts: %v", out)
	}
}

// The three routes over a real database: the two caller sets stay apart, every
// row carries a call site, and a citation is either the span's real digest or
// null.
func TestTheGraphEndpointsAnswerFromALiveCorpus(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "graphroutes", chunk.StrategyAST)
	h := liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor())
	add, other := liveGraph(t, st, repo)

	defs := body(t, get(t, h, "/api/repos/"+repo.ID+"/symbols?name=Add"))
	if defs["count"] != float64(1) || defs["matched"] != "exact" {
		t.Errorf("definitions of Add: %v", defs)
	}

	var callers struct {
		Symbol struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"symbol"`
		Depth   int `json:"depth"`
		Callers []struct {
			Symbol struct {
				Name string `json:"name"`
			} `json:"symbol"`
			Depth      int    `json:"depth"`
			Provenance string `json:"provenance"`
			Call       struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			} `json:"call"`
			Citation *rag.Citation `json:"citation"`
		} `json:"callers"`
		Approximate struct {
			MatchedOn string `json:"matched_on"`
			Count     int    `json:"count"`
			Callers   []struct {
				Symbol struct {
					Name string `json:"name"`
				} `json:"symbol"`
				ToName     string        `json:"to_name"`
				Provenance string        `json:"provenance"`
				Citation   *rag.Citation `json:"citation"`
			} `json:"callers"`
		} `json:"approximate"`
	}
	rec := get(t, h, "/api/repos/"+repo.ID+"/symbols/"+add.ID+"/callers")
	if rec.Code != http.StatusOK {
		t.Fatalf("callers: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &callers); err != nil {
		t.Fatal(err)
	}
	if callers.Symbol.Name != "Add" || callers.Depth != 1 {
		t.Errorf("callers of %+v at depth %d", callers.Symbol, callers.Depth)
	}
	// One resolved caller and one name-matched one, from two queries whose
	// answers the API refuses to merge (spec:84).
	if len(callers.Callers) != 1 || callers.Callers[0].Symbol.Name != "Use" ||
		callers.Callers[0].Provenance != "resolved" {
		t.Fatalf("precise callers %+v, want just Use", callers.Callers)
	}
	if callers.Callers[0].Call.Path != "calc/use.go" || callers.Callers[0].Call.Line == 0 {
		t.Errorf("call site %+v", callers.Callers[0].Call)
	}
	// The digest is the hash of the span's text as the chunker wrote it, read
	// back from the same database.
	cit := callers.Callers[0].Citation
	if cit == nil || cit.Digest == "" || cit.Permalink == "" || cit.Commit != repo.Commit {
		t.Fatalf("citation %+v", cit)
	}
	var digest string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT digest FROM spans WHERE repo_id = $1 AND path = 'calc/use.go' ORDER BY start_line LIMIT 1`,
		repo.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if cit.Digest != digest {
		t.Errorf("citation digest %s, want the span's %s", cit.Digest, digest)
	}
	if callers.Approximate.Count != 1 || callers.Approximate.MatchedOn != "name" ||
		callers.Approximate.Callers[0].Symbol.Name != "Other" ||
		callers.Approximate.Callers[0].ToName != "Add" ||
		callers.Approximate.Callers[0].Provenance != "syntactic" {
		t.Errorf("approximate %+v", callers.Approximate)
	}
	// Other has no span, so it has no digest to cite with.
	if callers.Approximate.Callers[0].Citation != nil {
		t.Errorf("the spanless caller cites %+v", callers.Approximate.Callers[0].Citation)
	}

	// And on its own route, the same null.
	one := body(t, get(t, h, "/api/repos/"+repo.ID+"/symbols/"+other.ID))
	if v, ok := one["citation"]; !ok || v != nil {
		t.Errorf("citation %v (present %v), want null", v, ok)
	}
	if _, ok := one["staleness"]; !ok {
		t.Errorf("a definition with no span lost its repository's staleness claim: %v", one)
	}

	// A symbol of another repository is a 404 here, not a read across a corpus
	// boundary the caller never named.
	otherRepo := indexFixture(t, st, "graphroutes2", chunk.StrategyAST)
	if rec := get(t, h, "/api/repos/"+otherRepo.ID+"/symbols/"+add.ID); rec.Code != http.StatusNotFound {
		t.Errorf("repo A's symbol read from repo B answered %d: %s", rec.Code, rec.Body)
	}
}
