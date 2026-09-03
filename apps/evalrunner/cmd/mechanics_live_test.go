//go:build live

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/corpus"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/fixture"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/source"
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/testdb"
)

// Two databases, because the arms live one per database and P2 proved live
// that a second arm in one database silently replaces the first. Neither is
// the database DATABASE_URL names.
var astDSN, windowDSN string

func TestMain(m *testing.M) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, "DATABASE_URL unset in CI — the live suite must never silently skip")
			os.Exit(1)
		}
		os.Exit(m.Run()) // every test skips; see dsns
	}
	code, err := testdb.Scratch(base, "codetrail_mech_ast", func(a string) int {
		astDSN = a
		inner, ierr := testdb.Scratch(base, "codetrail_mech_win", func(w string) int {
			windowDSN = w
			return m.Run()
		})
		if ierr != nil {
			fmt.Fprintln(os.Stderr, ierr)
			return 1
		}
		return inner
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func dsns(t *testing.T) (string, string) {
	t.Helper()
	if astDSN == "" || windowDSN == "" {
		t.Skip("set DATABASE_URL to run")
	}
	return astDSN, windowDSN
}

const (
	fixtureRoot = "../testdata/repo"
	fixCommit   = "2222222222222222222222222222222222222222"
)

// Each test gets its own remote, and so its own repo id. PutRepo adds and
// updates but never deletes, so two tests sharing a repository would leave the
// first one's file rows behind — which corpus.Verify then correctly refuses as
// a source mismatch, in a test that was not about that.
func remoteFor(t *testing.T) string { return "https://github.com/eval/" + t.Name() }

// The eval's geometry, reduced so the fixture's Sum sub-windows: a declaration
// under MaxDeclLines would never exercise the case whose symbol no span
// carries.
func fixtureChunk(s chunk.Strategy) chunk.Options {
	return chunk.Options{Strategy: s, WindowLines: 8, WindowOverlap: 2, MaxDeclLines: 10}
}

func checkout(t *testing.T, omit ...string) string {
	t.Helper()
	skip := map[string]bool{}
	for _, o := range omit {
		skip[o] = true
	}
	dst := t.TempDir()
	err := filepath.WalkDir(fixtureRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(fixtureRoot, p)
		if rerr != nil {
			return rerr
		}
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".txt")
		if skip[name] {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out := filepath.Join(dst, filepath.FromSlash(name))
		if merr := os.MkdirAll(filepath.Dir(out), 0o750); merr != nil {
			return merr
		}
		return os.WriteFile(out, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// corpora builds both arms from one checkout at one commit and returns the
// repo id they share.
func corpora(t *testing.T, root string, strip bool) string {
	t.Helper()
	a, w := dsns(t)
	remote := remoteFor(t)
	var repoID string
	for _, arm := range []struct {
		dsn      string
		strategy chunk.Strategy
	}{{a, chunk.StrategyAST}, {w, chunk.StrategyWindow}} {
		s, err := store.New(context.Background(), arm.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		id, _, ierr := fixture.Index(context.Background(), s, fixture.Options{
			Remote: remote, Ref: "main", Commit: fixCommit, Root: root,
			Chunk: fixtureChunk(arm.strategy), Strip: strip, Emb: embed.NewFake(store.EmbeddingDim),
		})
		if ierr != nil {
			t.Fatalf("indexing %s: %v", arm.strategy, ierr)
		}
		repoID = id
	}
	return repoID
}

func mechanicsFlags(t *testing.T, root, repoID, out string) flags {
	t.Helper()
	a, w := dsns(t)
	return flags{
		astDSN: a, windowDSN: w, repo: repoID, src: root, commit: fixCommit,
		out: out, ks: "1,5,10", floors: "-1,0,0.5,1",
		// Fixed, so two runs over one corpus are comparable byte for byte.
		now: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
}

// logged runs the harness and captures the per-arm progress lines, which are
// the only observable evidence that an arm was retrieved at all.
func logged(t *testing.T, f flags) (string, error) {
	t.Helper()
	var b strings.Builder
	f.log = &b
	err := run(context.Background(), f)
	return b.String(), err
}

func setEnv(t *testing.T) {
	t.Helper()
	// Spec §9: CI runs the harness with no model. Set here rather than in the
	// workflow, because newRetriever reads the ambient environment and other
	// suites in the same job assert the shipped defaults.
	t.Setenv("EMBED_PROVIDER", "fake")
	t.Setenv("STRIP_DOC_COMMENTS", "true")
	t.Setenv("CHUNK_WINDOW_LINES", "8")
	t.Setenv("CHUNK_WINDOW_OVERLAP", "2")
	t.Setenv("CHUNK_MAX_DECL_LINES", "10")
}

func onlyFile(t *testing.T, dir string) string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		found = append(found, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("%s holds %v, want exactly one artefact", dir, found)
	}
	return found[0]
}

// Determinism is asserted on the artefact, not on its summary: a ranked list
// permuted among tied spans produces the same MRR, and the claim §9 needs is
// that the *run* is reproducible.
func TestTheHarnessProducesByteIdenticalArtefactsAcrossTwoRunsLive(t *testing.T) {
	setEnv(t)
	root := checkout(t, "NOTES.md")
	repoID := corpora(t, root, true)

	outA, outB := t.TempDir(), t.TempDir()
	for _, out := range []string{outA, outB} {
		if err := run(context.Background(), mechanicsFlags(t, root, repoID, out)); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	one, err := os.ReadFile(onlyFile(t, outA))
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.ReadFile(onlyFile(t, outB))
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		a, b := strings.Split(string(one), "\n"), strings.Split(string(two), "\n")
		for i := range a {
			if i >= len(b) || a[i] != b[i] {
				t.Fatalf("the artefacts differ at line %d:\n run 1: %s\n run 2: %s", i+1, a[i], line(b, i))
			}
		}
		t.Fatalf("the artefacts differ in length: %d and %d bytes", len(one), len(two))
	}

	// And the corpus really does hold ties, or the comparison proves nothing:
	// embed.Fake is a hashed bag of words, so two spans with the same token
	// multiset produce the same vector.
	var r Run
	if uerr := json.Unmarshal(one, &r); uerr != nil {
		t.Fatal(uerr)
	}
	if len(r.Arms) != 2 || len(r.Arms[0].Cases) == 0 {
		t.Fatalf("the artefact holds %d arms", len(r.Arms))
	}
	t.Logf("%d cases, ast %d spans, window %d spans, %d bytes",
		len(r.Arms[0].Cases), r.Arms[0].Summary.Spans, r.Arms[1].Summary.Spans, len(one))
}

func line(ls []string, i int) string {
	if i < len(ls) {
		return ls[i]
	}
	return "<past the end>"
}

// The fixture's own ledger. Without it the fixture decays into "some Go files"
// the first time someone edits it, and every discriminating property in Tasks
// 1-4 quietly stops being exercised end to end.
func TestTheFixtureRepositoryExercisesEveryGoldenShapeLive(t *testing.T) {
	root := checkout(t)
	files, err := source.Read(context.Background(), root, source.Limits())
	if err != nil {
		t.Fatal(err)
	}
	var cases []golden.Case
	var stats golden.Stats
	for _, f := range files {
		if !f.Indexable() {
			continue
		}
		cs, st, gerr := golden.Generate(f.Path, f.Body)
		if gerr != nil {
			t.Fatal(gerr)
		}
		cases = append(cases, cs...)
		stats.Add(st)
	}
	byID := map[string]golden.Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}

	for _, want := range []string{
		"store.go:Store:decl",     // a documented exported type
		"store.go:Store.Get:decl", // a documented exported method
		"store.go:ErrGone:decl",   // a grouped block's own doc
		"store.go:ErrGone:spec",   // one spec inside that block
		"big.go:Sum:decl",         // sub-windowed under MaxDeclLines
	} {
		if _, ok := byID[want]; !ok {
			t.Errorf("the fixture no longer generates %q; it has %v", want, keys(byID))
		}
	}
	for _, unwanted := range []string{
		"store.go:Store.get:decl",    // exported receiver, unexported name
		"store.go:errHidden:spec",    // unexported spec in a documented block
		"store.go:undocumented:decl", // no doc comment
		"doc.go:repo:decl",           // a package doc is not an exported symbol
	} {
		if _, ok := byID[unwanted]; ok {
			t.Errorf("the fixture generates %q and must not", unwanted)
		}
	}
	if byID["store.go:ErrGone:decl"].Grouped != true {
		t.Error("the grouped block's case is not marked grouped")
	}
	if !byID["big.go:Sum:decl"].Moved() {
		t.Error("big.go:Sum's range does not move under stripping; the seven-line doc comment is gone")
	}
	if stats.Imports == 0 || stats.NoDoc == 0 || stats.Unexported == 0 || stats.SpecDocs == 0 {
		t.Errorf("the fixture no longer carries every shape: %+v", stats)
	}
	// doc.go and tools.go produce no AST spans and no cases; NOTES.md is not
	// Go at all.
	if stats.SkipReasons[golden.SkipNotGo] == 0 {
		t.Error("the fixture has no non-Go file; the leak class stripping cannot close is gone")
	}
}

func keys(m map[string]golden.Case) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Four surfaces, because P3's and P4's sweeps both found that a single side
// effect nothing reads back is the most common surviving mutant in this
// project.
func TestTheMechanicsRunIsLabelledNotQualityLive(t *testing.T) {
	setEnv(t)
	root := checkout(t, "NOTES.md")
	repoID := corpora(t, root, true)
	out := t.TempDir()
	if err := run(context.Background(), mechanicsFlags(t, root, repoID, out)); err != nil {
		t.Fatalf("run: %v", err)
	}
	path := onlyFile(t, out)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r Run
	if uerr := json.Unmarshal(b, &r); uerr != nil {
		t.Fatal(uerr)
	}
	got := [4]string{
		fmt.Sprint(r.Quality), r.Banner,
		filepath.Base(filepath.Dir(path)), filepath.Base(path),
	}
	if got[0] != "false" {
		t.Errorf("quality is %s, want false: this run used %s", got[0], r.Retrieval.EmbedModel)
	}
	if !strings.Contains(got[1], "§9") {
		t.Errorf("banner is %q; it must name spec §9", got[1])
	}
	if got[2] != DirMechanics {
		t.Errorf("the artefact landed in %q, want %q", got[2], DirMechanics)
	}
	if !strings.HasSuffix(got[3], "-"+FakeModel+".json") {
		t.Errorf("the filename is %q; the embed model is the one fact that decides whether a file is a measurement", got[3])
	}
	// The first key of the file, so a reader who greps or diffs hits the label
	// before any number.
	if first := strings.SplitN(strings.TrimPrefix(string(b), "{\n"), "\n", 2)[0]; !strings.Contains(first, `"quality"`) {
		t.Errorf("the file's first key is %q, want quality", strings.TrimSpace(first))
	}
}

func TestBothArmsAreBuiltFromOneCheckoutAtOneCommitLive(t *testing.T) {
	setEnv(t)
	root := checkout(t, "NOTES.md")
	repoID := corpora(t, root, true)
	out := t.TempDir()
	if err := run(context.Background(), mechanicsFlags(t, root, repoID, out)); err != nil {
		t.Fatalf("run: %v", err)
	}
	var r Run
	b, err := os.ReadFile(onlyFile(t, out))
	if err != nil {
		t.Fatal(err)
	}
	if uerr := json.Unmarshal(b, &r); uerr != nil {
		t.Fatal(uerr)
	}
	if r.Commit != fixCommit {
		t.Errorf("the run is at %s, want %s", r.Commit, fixCommit)
	}
	if r.Corpus.Files["ast"] != r.Corpus.Files["window"] {
		t.Errorf("the arms hold %v files", r.Corpus.Files)
	}
	if r.Corpus.Spans["ast"] == r.Corpus.Spans["window"] {
		t.Errorf("both arms hold %d spans; the fixture no longer separates them", r.Corpus.Spans["ast"])
	}
	if len(r.ArmConfig) != 2 {
		t.Fatalf("the artefact records %d arm configurations", len(r.ArmConfig))
	}
	for _, ac := range r.ArmConfig {
		if !ac.Strip {
			t.Errorf("arm %s records strip=false; the whole comparison is over the stripped corpus", ac.Name)
		}
		if ac.Database == "" || strings.Contains(ac.Database, "@") {
			t.Errorf("arm %s records database %q", ac.Name, ac.Database)
		}
	}
	if r.ArmConfig[0].Database == r.ArmConfig[1].Database {
		t.Errorf("both arms record database %q", r.ArmConfig[0].Database)
	}
	// Both arms saw the same questions, in the same order.
	if len(r.Arms[0].Cases) != len(r.Arms[1].Cases) {
		t.Fatalf("the arms were asked %d and %d questions", len(r.Arms[0].Cases), len(r.Arms[1].Cases))
	}
	for i := range r.Arms[0].Cases {
		if r.Arms[0].Cases[i].CaseID != r.Arms[1].Cases[i].CaseID {
			t.Fatalf("case %d is %q in the ast arm and %q in the window arm",
				i, r.Arms[0].Cases[i].CaseID, r.Arms[1].Cases[i].CaseID)
		}
	}
}

// A leaking corpus produces a complete artefact full of numbers that mean
// nothing, and an artefact that exists is an artefact someone quotes. The
// assertion is on the absence of the output file, not on the exit code: a
// runner that probed late and then failed still leaves the file behind.
func TestTheRunnerRefusesBeforeItRetrievesWhenTheProbeFindsLive(t *testing.T) {
	setEnv(t)
	// NOTES.md is in, and it repeats one symbol's prose verbatim. StripDocs
	// passes a non-Go file through unchanged, so the corpus leaks however well
	// the Go side was stripped.
	root := checkout(t)
	repoID := corpora(t, root, true)
	out := t.TempDir()

	log, err := logged(t, mechanicsFlags(t, root, repoID, out))
	if err == nil {
		t.Fatal("the run succeeded over a leaking corpus")
	}
	if !strings.Contains(err.Error(), "NOTES.md") || !strings.Contains(err.Error(), "was not stripped") {
		t.Errorf("the refusal is %q; it must name the span and say what is wrong", err)
	}
	entries, rerr := os.ReadDir(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("the output directory holds %v after a refused run; want no artefact written", entries)
	}
	// Before it *retrieves*, not merely before it writes. A mutant that probed
	// after retrieving but before writing leaves no artefact either, so the
	// assertion above cannot tell the two apart — measured, it survived.
	if log != "" {
		t.Errorf("the runner printed %q; it retrieved before the probe refused the corpus", log)
	}
}

func TestTheRunnerRefusesTwoArmsInOneDatabaseLive(t *testing.T) {
	setEnv(t)
	root := checkout(t, "NOTES.md")
	repoID := corpora(t, root, true)
	f := mechanicsFlags(t, root, repoID, t.TempDir())
	f.windowDSN = f.astDSN
	err := run(context.Background(), f)
	if err == nil {
		t.Fatal("the run accepted two arms in one database")
	}
	if !strings.Contains(err.Error(), corpus.ErrSameDatabase.Error()) {
		t.Errorf("the refusal is %q, want ErrSameDatabase", err)
	}
}

func TestTheRunnerRefusesACommitTheArmsAreNotAtLive(t *testing.T) {
	setEnv(t)
	root := checkout(t, "NOTES.md")
	repoID := corpora(t, root, true)
	f := mechanicsFlags(t, root, repoID, t.TempDir())
	f.commit = "3333333333333333333333333333333333333333"
	if err := run(context.Background(), f); err == nil || !strings.Contains(err.Error(), fixCommit) {
		t.Errorf("the run gave %v; want a refusal naming the commit the arms are actually at", err)
	}
}

// The floor sweep has to be recomputable from the committed file with no
// embedder in the loop, which is what spec:249's "recorded with the numbers
// that produced it" means if it means anything.
func TestTheSweepIsRecomputableFromTheArtefactLive(t *testing.T) {
	setEnv(t)
	root := checkout(t, "NOTES.md")
	repoID := corpora(t, root, true)
	out := t.TempDir()
	if err := run(context.Background(), mechanicsFlags(t, root, repoID, out)); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(onlyFile(t, out))
	if err != nil {
		t.Fatal(err)
	}
	var r Run
	if uerr := json.Unmarshal(b, &r); uerr != nil {
		t.Fatal(uerr)
	}
	for _, arm := range r.Arms {
		if len(arm.Cases) == 0 {
			t.Fatalf("arm %s recorded no cases; the sweep cannot be recomputed", arm.Name)
		}
		outcomes := make([]metric.Outcome, 0, len(arm.Cases))
		for _, c := range arm.Cases {
			outcomes = append(outcomes, c.Outcomes())
		}
		floors := make([]float64, 0, len(arm.Summary.Sweep))
		for _, c := range arm.Summary.Sweep {
			floors = append(floors, c.Floor)
		}
		got := metric.Sweep(outcomes, floors)
		for i := range got {
			if fmt.Sprint(got[i]) != fmt.Sprint(arm.Summary.Sweep[i]) {
				t.Errorf("arm %s: the sweep recomputed from the file is %+v at floor %v, and the file records %+v",
					arm.Name, got[i], floors[i], arm.Summary.Sweep[i])
			}
		}
	}
}
