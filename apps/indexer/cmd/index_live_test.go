//go:build live

package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/clone"
	"github.com/mralaminahamed/codetrail/apps/indexer/internal/walk"
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/testdb"
)

// scratchDSN names a database this suite creates for itself.
//
// Not the shared one DATABASE_URL points at: `go test -tags=live ./...` runs the
// packages at once and the eviction test below clears every repo. Every live
// suite in the tree now does the same, through the same helper, so no suite can
// empty a table in a database a reader pointed it at.
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
		os.Exit(m.Run()) // every test skips; see liveDSN
	}
	// Spec §9: the whole harness runs with no model. Here it is also the
	// oracle — every span is checked against the vector its own text hashes to
	// — so a real embedder would not make this suite slow, it would make that
	// assertion unwritable. Defaulted rather than demanded, and refused rather
	// than overridden, so neither a bare `go test -tags=live ./...` nor a shell
	// that already points at Ollama can pick the wrong one silently.
	switch p := os.Getenv("EMBED_PROVIDER"); p {
	case "":
		os.Setenv("EMBED_PROVIDER", "fake")
	case "fake":
	default:
		fmt.Fprintf(os.Stderr, "EMBED_PROVIDER=%q: the live suite runs on the fake embedder\n", p)
		os.Exit(1)
	}
	code, err := testdb.Scratch(base, "codetrail_indexer", func(d string) int {
		scratchDSN = d
		return m.Run()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func liveDSN(t *testing.T) string {
	t.Helper()
	if scratchDSN == "" {
		t.Skip("set DATABASE_URL to run")
	}
	return scratchDSN
}

func liveStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(context.Background(), liveDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// fixtureRoot is the tree these jobs index, committed so a line range means the
// same thing on every machine.
//
// Every file carries a trailing ".txt". Measured: `gofmt -l apps` walks testdata
// and reports Go it finds there, so the deliberately unformatted and unparseable
// fixtures would fail the lint gate. `go build ./...` and `go vet ./...` ignore
// testdata entirely, so that half is not a reason. The suffix comes off on the
// way into the checkout because walk classifies by extension and ".go.txt" is no
// language at all.
const fixtureRoot = "testdata/repo"

// sentinel appears only inside Go doc comments in the fixture. Nothing in the
// markdown or the YAML carries it, so a stripped corpus that still contains it
// can only have got it from prose StripDocs was supposed to remove.
const sentinel = "CODETRAILDOC"

func copyFixture(t *testing.T, dst string) {
	t.Helper()
	err := filepath.WalkDir(fixtureRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(fixtureRoot, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, strings.TrimSuffix(rel, ".txt"))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		return os.WriteFile(out, body, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fixtureBytes is the file a span cites, as it exists at the indexed commit.
// The checkout is a byte-for-byte copy of this tree and is deleted when the job
// ends, so the committed file is both the same bytes and the durable one.
func fixtureBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, filepath.FromSlash(path)+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fileRow struct {
	Path, Blob, Lang string
	Lines            int
}

type spanRow struct {
	ID, Path, Symbol, Text, Digest, Model string
	Kind                                  models.SpanKind
	Start, End, Dim                       int
	Vec                                   []float32
}

// sig is the part of a span a reviewer can check by opening the file: where it
// points and what it claims to be.
func (s spanRow) sig() string {
	return fmt.Sprintf("%s|%s|%s|%d|%d", s.Path, s.Kind, s.Symbol, s.Start, s.End)
}

func sigs(spans []spanRow) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.sig())
	}
	return out
}

type liveJob struct {
	name     string
	strategy chunk.Strategy
	strip    bool
	// prepare adds to the checkout after the fixture is copied, for the things
	// a committed tree cannot hold portably.
	prepare func(t *testing.T, dir string)
}

type liveRun struct {
	repoID string
	files  []fileRow
	spans  []spanRow
}

// indexLive runs one whole job against Postgres: the real walk, the real
// chunker and the real store, with the fake embedder spec §9 puts in CI and a
// clone that lays down the committed fixture instead of reaching the network.
func indexLive(t *testing.T, st *store.Store, j liveJob) liveRun {
	t.Helper()
	ctx := context.Background()

	q := &fakeQueue{}
	ix, _ := testIndexer(t, q)
	// The fixture is a repository, not a snippet: big.go alone is 2,466 bytes,
	// past testIndexer's 1KB cap, and a file the walk skips for size is a
	// missing span rather than a failure that says so.
	ix.lim.walk = walk.Limits{MaxFiles: 64, MaxFileBytes: 1 << 20}
	ix.lim.clone.Deadline = 2 * time.Minute
	// Smaller than the fixture's span count, so a batch loop that stopped after
	// the first request leaves the rest with no vector. Measured: PutSpans then
	// refuses the whole job, naming big.go:93-132 as the first span with 0
	// components.
	ix.lim.embedBatch = 4
	// The real eviction rather than a stub, so every job here runs the whole of
	// runJob — but with a keep above the number of repos this suite creates, or
	// a later test would evict an earlier one's rows out from under it. runJob
	// logs an eviction failure and drops it, so this is coverage of the call and
	// not an assertion about it; the cascade is checked below.
	ix.lim.keepRepos = 100
	opt := chunk.Defaults()
	opt.Strategy = j.strategy
	ix.opt, ix.strip = opt, j.strip
	// From the environment, through the same constructor main uses: an
	// embedder wired by hand here would prove the pipeline works with an
	// embedder production never builds.
	emb, err := newEmbedder(ix.lim.clone.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	ix.emb = emb
	ix.clone = func(_ context.Context, _, _, dir string, _ clone.Limits) (clone.Result, error) {
		copyFixture(t, dir)
		if j.prepare != nil {
			j.prepare(t, dir)
		}
		return clone.Result{Dir: dir, Commit: testCommit, Bytes: 1}, nil
	}
	ix.walk = walk.Files
	ix.put, ix.putSpans, ix.evict = st.PutRepo, st.PutSpans, st.Evict

	remote := "https://github.com/codetrail-live/" + j.name
	ix.runJob(ctx, jobs.Job{
		ID: "live-" + j.name, Remote: remote, Ref: "main",
		Status: jobs.StatusLeased, Attempts: 1,
	})
	if len(q.failed) > 0 {
		t.Fatalf("the job failed instead of indexing: %s", q.failed[0].reason)
	}
	if len(q.completed) != 1 {
		t.Fatalf("the job was neither completed nor failed: %v", q.completed)
	}

	var repoID string
	if err := st.Pool().QueryRow(ctx, `SELECT id FROM repos WHERE remote = $1`, remote).Scan(&repoID); err != nil {
		t.Fatalf("no repo row for %s: %v", remote, err)
	}
	return liveRun{repoID: repoID, files: readFiles(t, st, repoID), spans: readSpans(t, st, repoID)}
}

func readFiles(t *testing.T, st *store.Store, repoID string) []fileRow {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(),
		`SELECT path, blob, lang, lines FROM files WHERE repo_id = $1 ORDER BY path COLLATE "C"`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []fileRow
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.Path, &f.Blob, &f.Lang, &f.Lines); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// readSpans reads the columns a citation is made of, straight out of Postgres
// rather than out of the value the indexer handed the store — the round trip is
// part of what this suite is proving.
func readSpans(t *testing.T, st *store.Store, repoID string) []spanRow {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `
		SELECT id, path, kind, symbol, start_line, end_line, text, digest,
		       embed_model, embed_dim, embedding::text
		FROM spans WHERE repo_id = $1 ORDER BY path COLLATE "C", start_line, end_line`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []spanRow
	for rows.Next() {
		var s spanRow
		var vec *string
		if err := rows.Scan(&s.ID, &s.Path, &s.Kind, &s.Symbol, &s.Start, &s.End,
			&s.Text, &s.Digest, &s.Model, &s.Dim, &vec); err != nil {
			t.Fatal(err)
		}
		if vec == nil {
			// A null embedding is a span no query can ever reach, and nothing
			// downstream would report it as missing.
			t.Fatalf("span %s (%s:%d-%d) has no embedding", s.ID, s.Path, s.Start, s.End)
		}
		s.Vec = parseVec(t, *vec)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func parseVec(t *testing.T, s string) []float32 {
	t.Helper()
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 32)
		if err != nil {
			t.Fatalf("embedding component %d: %v", i, err)
		}
		out[i] = float32(f)
	}
	return out
}

// The spans the AST arm owes this fixture, by hand from the committed files.
// Counting them would pass against a chunker that put every range one line out;
// naming them is what makes this an assertion about citations.
var wantASTSpans = []string{
	"README.md|file||1|4",
	"big.go|file||3|42",
	"big.go|file||33|72",
	"big.go|file||63|102",
	"big.go|file||93|132",
	"big.go|file||123|162",
	"big.go|file||153|192",
	"big.go|file||183|208",
	"calc/broken.go|file||1|4",
	"calc/calc.go|const|Base|5|6",
	"calc/calc.go|var|Add|8|12",
	"calc/calc.go|type|Machine|14|17",
	"calc/calc.go|func|Machine.Push|19|23",
	"calc/calc.go|func|Render|25|28",
	"config.yaml|file||1|3",
}

// The same bytes under the baseline arm. big.go tiles from line 1 rather than
// from its doc comment, calc.go is one window instead of five declarations, and
// doc.go and tools.go appear at all.
var wantWindowSpans = []string{
	"README.md|file||1|4",
	"big.go|file||1|40",
	"big.go|file||31|70",
	"big.go|file||61|100",
	"big.go|file||91|130",
	"big.go|file||121|160",
	"big.go|file||151|190",
	"big.go|file||181|208",
	"calc/broken.go|file||1|4",
	"calc/calc.go|file||1|28",
	"config.yaml|file||1|3",
	"doc.go|file||1|5",
	"tools.go|file||1|9",
}

// Every regular file the walk saw, span-bearing or not. go.mod and logo.png are
// here on purpose: a file row without spans is what says the walk covered the
// repository even where the chunker declined it.
var wantFiles = []fileRow{
	{Path: "README.md", Lang: "markdown", Lines: 4},
	{Path: "big.go", Lang: "go", Lines: 208},
	{Path: "calc/broken.go", Lang: "go", Lines: 4},
	{Path: "calc/calc.go", Lang: "go", Lines: 28},
	{Path: "config.yaml", Lang: "yaml", Lines: 3},
	{Path: "doc.go", Lang: "go", Lines: 5},
	{Path: "go.mod", Lang: "", Lines: 3},
	{Path: "logo.png", Lang: "", Lines: 2},
	{Path: "tools.go", Lang: "go", Lines: 9},
}

// The phase's headline claim, end to end: a repository goes in and citable
// spans come out — these spans, with the columns a citation is made of.
func TestIndexProducesTheExpectedSpansLive(t *testing.T) {
	st := liveStore(t)
	run := indexLive(t, st, liveJob{name: "spans", strategy: chunk.StrategyAST})

	if got := sigs(run.spans); !slices.Equal(got, wantASTSpans) {
		t.Fatalf("spans:\n got %s\nwant %s", strings.Join(got, "\n      "), strings.Join(wantASTSpans, "\n      "))
	}

	fake := embed.NewFake(store.EmbeddingDim)
	for _, s := range run.spans {
		if s.Digest != store.Digest(s.Text) {
			t.Errorf("%s: digest does not hash the text stored beside it", s.sig())
		}
		if s.Model != fake.Model() || s.Dim != store.EmbeddingDim {
			t.Errorf("%s: embed_model=%q embed_dim=%d, want %q and %d",
				s.sig(), s.Model, s.Dim, fake.Model(), store.EmbeddingDim)
		}
		// Not "a vector is present": the vector of *this* span's text. A batch
		// loop off by one files each span's text under its neighbour's
		// embedding, and every row still looks fully populated.
		want, err := fake.Embed(context.Background(), []string{s.Text})
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Vec) != store.EmbeddingDim {
			t.Fatalf("%s: %d components, want %d", s.sig(), len(s.Vec), store.EmbeddingDim)
		}
		for i := range want[0] {
			if diff := s.Vec[i] - want[0][i]; diff > 1e-6 || diff < -1e-6 {
				t.Fatalf("%s: component %d is %v, but this span's text embeds to %v",
					s.sig(), i, s.Vec[i], want[0][i])
			}
		}
	}

	if len(run.files) != len(wantFiles) {
		t.Fatalf("%d file rows, want %d: %+v", len(run.files), len(wantFiles), run.files)
	}
	for i, w := range wantFiles {
		got := run.files[i]
		if got.Path != w.Path || got.Lang != w.Lang || got.Lines != w.Lines {
			t.Errorf("file row %d: got %+v, want %+v", i, got, w)
		}
		if want := blobHash(fixtureBytes(t, w.Path)); got.Blob != want {
			t.Errorf("%s: blob %s, want git's %s", w.Path, got.Blob, want)
		}
	}
}

// The property every citation rests on: the range brackets the bytes the span
// claims. models.Span calls an off-by-one here a citation pointing at the wrong
// code, and this is where that is checked against the file rather than against
// the chunker's own arithmetic.
//
// Half of the property, precisely: a chunker that shifted the range and the
// text together would still be self-consistent and would pass here. Which lines
// each span is supposed to name is pinned by wantASTSpans, computed by hand from
// the committed files; this is what stops the two drifting apart.
func TestEverySpanBracketsTheBytesItCitesLive(t *testing.T) {
	st := liveStore(t)
	// Only the unstripped runs: a stripped span's text comes from bytes
	// StripDocs produced, which are not the bytes on disk, and production —
	// the configuration whose citations a reader is shown — does not strip.
	for _, strategy := range []chunk.Strategy{chunk.StrategyAST, chunk.StrategyWindow} {
		run := indexLive(t, st, liveJob{name: "cites-" + string(strategy), strategy: strategy})
		if len(run.spans) == 0 {
			t.Fatalf("%s produced no spans to check", strategy)
		}
		for _, s := range run.spans {
			if !bracketsExactly(fixtureBytes(t, s.Path), s.Text, s.Start, s.End) {
				t.Errorf("%s: lines %d-%d of %s are not the bytes this span stores:\n%s",
					strategy, s.Start, s.End, s.Path, s.Text)
			}
		}
	}
}

// bracketsExactly reports whether text occupies exactly lines start..end of
// body, 1-based and inclusive.
//
// By byte offset, not by splitting body into lines: re-running the chunker's
// own line arithmetic here would agree with it about an off-by-one. An
// occurrence qualifies only if it begins a line, ends one, has start-1 newlines
// before it and end-start inside it — so a range shifted either way, or one
// line too wide, has no qualifying occurrence at all.
func bracketsExactly(body []byte, text string, start, end int) bool {
	t := []byte(text)
	if len(t) == 0 {
		return false
	}
	for off := 0; off+len(t) <= len(body); {
		i := bytes.Index(body[off:], t)
		if i < 0 {
			return false
		}
		at := off + i
		startsLine := at == 0 || body[at-1] == '\n'
		endsLine := at+len(t) == len(body) || body[at+len(t)] == '\n'
		if startsLine && endsLine &&
			bytes.Count(body[:at], []byte{'\n'}) == start-1 &&
			bytes.Count(t, []byte{'\n'}) == end-start {
			return true
		}
		off = at + 1
	}
	return false
}

// Spec §3: re-indexing the same commit writes the same rows. Ids, not counts —
// two runs can agree on how many spans there are and disagree on which.
func TestReindexingConvergesLive(t *testing.T) {
	st := liveStore(t)
	first := indexLive(t, st, liveJob{name: "converge", strategy: chunk.StrategyAST})
	second := indexLive(t, st, liveJob{name: "converge", strategy: chunk.StrategyAST})

	if first.repoID != second.repoID {
		t.Fatalf("one commit got two repo ids: %s and %s", first.repoID, second.repoID)
	}
	ids := func(spans []spanRow) []string {
		out := make([]string, 0, len(spans))
		for _, s := range spans {
			out = append(out, s.ID)
		}
		slices.Sort(out)
		return out
	}
	a, b := ids(first.spans), ids(second.spans)
	if !slices.Equal(a, b) {
		t.Fatalf("the second index wrote a different set of spans: %d ids vs %d", len(a), len(b))
	}
	// The ids are a hash of the range and the text, so equal ids with unequal
	// rows would mean the rewrite dropped a column rather than the row.
	if !slices.Equal(sigs(first.spans), sigs(second.spans)) {
		t.Fatal("the same span ids came back with different ranges or kinds")
	}
	if len(slices.Compact(slices.Clone(a))) != len(a) {
		t.Fatalf("re-indexing doubled the corpus: %d ids, %d distinct", len(a), len(slices.Compact(slices.Clone(a))))
	}
	n, err := st.CountSpans(context.Background(), second.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(wantASTSpans) {
		t.Fatalf("%d spans after two indexes of one commit, want %d", n, len(wantASTSpans))
	}
}

// The eval's two arms over one corpus. Not a P6 test: it is the proof that the
// seam built in Task 1 carries a second arm, which is the thing a retrofit
// would have got wrong.
func TestBothStrategiesChunkTheSameBytesDifferentlyLive(t *testing.T) {
	st := liveStore(t)
	ast := indexLive(t, st, liveJob{name: "arms-ast", strategy: chunk.StrategyAST})
	win := indexLive(t, st, liveJob{name: "arms-window", strategy: chunk.StrategyWindow})

	if !slices.Equal(sigs(ast.spans), wantASTSpans) {
		t.Fatalf("ast arm: %v", sigs(ast.spans))
	}
	if !slices.Equal(sigs(win.spans), wantWindowSpans) {
		t.Fatalf("window arm: %v", sigs(win.spans))
	}

	kinds := map[models.SpanKind]int{}
	for _, s := range win.spans {
		kinds[s.Kind]++
		if s.Symbol != "" {
			t.Errorf("the window arm named a symbol on %s: %q", s.sig(), s.Symbol)
		}
	}
	if kinds[models.KindFile] != len(win.spans) {
		t.Fatalf("the window arm produced %d spans that are not kind=file", len(win.spans)-kinds[models.KindFile])
	}

	// The AST arm carries kind=file rows too — sub-windowed big.go and
	// unparseable broken.go — so nothing may read the arm off the kind column.
	astKinds := map[models.SpanKind]int{}
	for _, s := range ast.spans {
		astKinds[s.Kind]++
	}
	for _, k := range []models.SpanKind{models.KindFunc, models.KindType, models.KindConst, models.KindVar, models.KindFile} {
		if astKinds[k] == 0 {
			t.Errorf("the AST arm produced no %s span; kinds were %v", k, astKinds)
		}
	}

	// Measured, and not what this plan predicted: the arms do *not* cover the
	// same files. A package-doc-only file and an import-only one have no
	// top-level declaration the AST arm will emit, so they are in the baseline
	// corpus and absent from the AST one.
	covered := func(spans []spanRow) []string {
		var out []string
		for _, s := range spans {
			if !slices.Contains(out, s.Path) {
				out = append(out, s.Path)
			}
		}
		slices.Sort(out)
		return out
	}
	onlyWindow := []string{"doc.go", "tools.go"}
	for _, p := range onlyWindow {
		if slices.Contains(covered(ast.spans), p) {
			t.Errorf("%s now has AST spans; the corpus asymmetry the README records is stale", p)
		}
		if !slices.Contains(covered(win.spans), p) {
			t.Errorf("%s has no window spans either, so it is unretrievable in both arms", p)
		}
	}
	if !slices.Equal(covered(ast.spans), slices.DeleteFunc(covered(win.spans), func(p string) bool {
		return slices.Contains(onlyWindow, p)
	})) {
		t.Errorf("the arms differ on more than %v: ast %v, window %v",
			onlyWindow, covered(ast.spans), covered(win.spans))
	}

	// Both arms saw the whole repository even where only one of them chunked it.
	if !slices.Equal(ast.files, win.files) {
		t.Errorf("the arms wrote different file rows:\n%+v\n%+v", ast.files, win.files)
	}
}

// Open Question 6, live: SpanID has no room for a strategy, so two arms in one
// database are one arm. Pinned because P6 has to put them in separate databases
// and nothing else in the tree says why.
func TestASecondArmReplacesTheFirstInOneDatabaseLive(t *testing.T) {
	st := liveStore(t)
	ast := indexLive(t, st, liveJob{name: "collide", strategy: chunk.StrategyAST})
	win := indexLive(t, st, liveJob{name: "collide", strategy: chunk.StrategyWindow})
	if ast.repoID != win.repoID {
		t.Fatalf("the two arms landed in different repos: %s and %s", ast.repoID, win.repoID)
	}
	n, err := st.CountSpans(context.Background(), win.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(wantWindowSpans) {
		t.Fatalf("%d spans after both arms, want the window arm's %d alone", n, len(wantWindowSpans))
	}
}

// The corpus the eval indexes is not the corpus production indexes, and the
// difference is exactly the doc comments (spec §5) — with the line numbers
// unmoved, so a range still addresses the same physical lines.
func TestStrippedCorpusHoldsNoDocProseLive(t *testing.T) {
	st := liveStore(t)
	kept := indexLive(t, st, liveJob{name: "kept", strategy: chunk.StrategyWindow})
	stripped := indexLive(t, st, liveJob{name: "stripped", strategy: chunk.StrategyWindow, strip: true})

	// Both halves. Without the first, a fixture with no doc comments at all
	// would pass the second and prove nothing.
	var keptHits int
	for _, s := range kept.spans {
		if strings.Contains(s.Text, sentinel) {
			keptHits++
		}
	}
	if keptHits == 0 {
		t.Fatalf("no unstripped span contains %s; the fixture has no doc prose to strip", sentinel)
	}
	for _, s := range stripped.spans {
		if strings.Contains(s.Text, sentinel) {
			t.Errorf("stripped span %s still holds doc prose:\n%s", s.sig(), s.Text)
		}
	}

	// Same ranges, different bytes: blanking keeps the newlines, so the two
	// corpora tile identically and a citation into either names the same lines.
	// broken.go is the exception and is asserted rather than excused — Go that
	// will not parse cannot be stripped, so it is absent from the eval corpus.
	keptText := map[string]string{}
	for _, s := range kept.spans {
		if s.Path != "calc/broken.go" {
			keptText[s.sig()] = s.Text
		}
	}
	if len(keptText) == len(kept.spans) {
		t.Fatal("calc/broken.go had no spans unstripped either, so its absence below proves nothing")
	}
	if !slices.Equal(slices.Sorted(maps.Keys(keptText)), slices.Sorted(slices.Values(sigs(stripped.spans)))) {
		t.Fatalf("stripping moved the ranges:\n    kept %v\nstripped %v",
			slices.Sorted(maps.Keys(keptText)), sigs(stripped.spans))
	}
	// Same ranges is only half of it: identical text would mean the ranges
	// survived because nothing happened.
	var changed int
	for _, s := range stripped.spans {
		if s.Text != keptText[s.sig()] {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("every stripped span has its unstripped twin's text, so nothing was stripped")
	}
}

// A link in a checkout never becomes a span, end to end.
//
// Not Task 6's M11, which this plan predicted this test would kill. Measured:
// replacing the chunk pass's walk.ReadRegular with os.ReadFile leaves this test
// green, because the walk drops anything that is a link at listing time and the
// second read therefore never sees one. Only the fake walk in main_test.go can
// hand over a listing that lies, and that is where M11 dies.
//
// What this does pin is the pair of guards in walk.Files, at the corpus rather
// than at the reader. Measured: dropping the listing's IsRegular check alone
// changes nothing, dropping O_NOFOLLOW alone changes nothing, and dropping both
// puts "func P" from outside the checkout into the corpus as escape.go:3-4.
func TestNoSpanHoldsALinkTargetLive(t *testing.T) {
	st := liveStore(t)
	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("package p\n\n// LEAKEDSECRET\nfunc P() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := indexLive(t, st, liveJob{name: "symlink", strategy: chunk.StrategyAST,
		prepare: func(t *testing.T, dir string) {
			if err := os.Symlink(outside, filepath.Join(dir, "escape.go")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}})

	for _, s := range run.spans {
		if strings.Contains(s.Text, "LEAKEDSECRET") {
			t.Fatalf("span %s holds the link target's bytes", s.sig())
		}
	}
	for _, f := range run.files {
		if f.Path == "escape.go" {
			t.Fatal("the link got a file row, so something read through it")
		}
	}
	// The corpus is otherwise untouched: a guard that dropped the whole
	// repository would also pass the two assertions above.
	if !slices.Equal(sigs(run.spans), wantASTSpans) {
		t.Fatalf("the link changed the rest of the corpus: %v", sigs(run.spans))
	}
}

// Eviction is one DELETE that cascades (spec §3), and spans are a new dependent
// of repos.
func TestEvictionRemovesSpansEndToEndLive(t *testing.T) {
	ctx := context.Background()
	st := liveStore(t)
	run := indexLive(t, st, liveJob{name: "evict", strategy: chunk.StrategyAST})
	if n, err := st.CountSpans(ctx, run.repoID); err != nil || n == 0 {
		t.Fatalf("nothing to evict: %d spans, %v", n, err)
	}
	if _, err := st.Evict(ctx, 0); err != nil {
		t.Fatal(err)
	}
	n, err := st.CountSpans(ctx, run.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d spans outlived their repo", n)
	}
	if got := readFiles(t, st, run.repoID); len(got) != 0 {
		t.Fatalf("%d file rows outlived their repo", len(got))
	}
}
