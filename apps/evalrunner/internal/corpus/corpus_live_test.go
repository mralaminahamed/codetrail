//go:build live

package corpus

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/fixture"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/source"
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/testdb"
)

// This phase indexes two arms into two databases, so the suite nests
// testdb.Scratch twice. Neither is the database DATABASE_URL names: a suite
// that cleared whole tables in a developer's own database while printing "ok"
// is what testdb exists to prevent.
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
	code, err := testdb.Scratch(base, "codetrail_eval_ast", func(a string) int {
		astDSN = a
		inner, ierr := testdb.Scratch(base, "codetrail_eval_window", func(w string) int {
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
	fixtureRoot = "../../testdata/repo"
	decoyRoot   = "../../testdata/other"
	commit      = "1111111111111111111111111111111111111111"
)

// The eval's own geometry, reduced so the fixture's Sum sub-windows: a
// declaration under MaxDeclLines would never exercise the case whose symbol no
// span carries.
func opts(s chunk.Strategy) chunk.Options {
	return chunk.Options{Strategy: s, WindowLines: 8, WindowOverlap: 2, MaxDeclLines: 10}
}

// checkout copies the fixture tree into a scratch directory, dropping the
// .txt suffix, and optionally leaving files out.
func checkout(t *testing.T, root string, omit ...string) string {
	t.Helper()
	skip := map[string]bool{}
	for _, o := range omit {
		skip[o] = true
	}
	dst := t.TempDir()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
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

func open(t *testing.T, dsn string) *store.Store {
	t.Helper()
	s, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// build indexes one checkout into one database and returns the arm.
func build(t *testing.T, dsn, name, remote, root string, strategy chunk.Strategy, strip bool) Arm {
	t.Helper()
	s := open(t, dsn)
	repoID, _, err := fixture.Index(context.Background(), s, fixture.Options{
		Remote: remote, Ref: "main", Commit: commit, Root: root,
		Chunk: opts(strategy), Strip: strip, Emb: embed.NewFake(store.EmbeddingDim),
	})
	if err != nil {
		t.Fatalf("indexing %s: %v", name, err)
	}
	return Arm{Name: name, DSN: dsn, RepoID: repoID, Read: FromStore(s)}
}

// cases generates the golden set from the same checkout the corpus was built
// from, which is what the source binding then proves.
func cases(t *testing.T, root string) ([]golden.Case, map[string]string) {
	t.Helper()
	files, err := source.Read(context.Background(), root, source.Limits())
	if err != nil {
		t.Fatal(err)
	}
	var out []golden.Case
	for _, f := range files {
		if !f.Indexable() {
			continue
		}
		cs, _, gerr := golden.Generate(f.Path, f.Body)
		if gerr != nil {
			t.Fatalf("Generate(%s): %v", f.Path, gerr)
		}
		out = append(out, cs...)
	}
	return out, source.Blobs(files)
}

func TestTheTwoArmsAgreeOnEveryFileBlobLive(t *testing.T) {
	a, w := dsns(t)
	root := checkout(t, fixtureRoot)
	ast := build(t, a, "ast", "https://github.com/eval/repo", root, chunk.StrategyAST, true)
	win := build(t, w, "window", "https://github.com/eval/repo", root, chunk.StrategyWindow, true)
	_, blobs := cases(t, root)

	rep, err := Verify(context.Background(), ast, win, blobs)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Commit != commit {
		t.Errorf("report commit %s, want %s", rep.Commit, commit)
	}
	if rep.Files["ast"] != rep.Files["window"] || rep.Files["ast"] == 0 {
		t.Errorf("file counts are %v", rep.Files)
	}
	// The arms have to actually differ, or there is nothing to compare.
	if rep.Spans["ast"] == rep.Spans["window"] {
		t.Errorf("both arms produced %d spans; the fixture no longer separates them", rep.Spans["ast"])
	}
	t.Logf("ast %d spans, window %d spans, %d shared ids, %d files",
		rep.Spans["ast"], rep.Spans["window"], rep.SharedSpanIDs, rep.Files["ast"])
}

func TestASourceTreeThatNoArmIndexedIsRefusedLive(t *testing.T) {
	a, w := dsns(t)
	root := checkout(t, fixtureRoot)
	ast := build(t, a, "ast", "https://github.com/eval/repo", root, chunk.StrategyAST, true)
	win := build(t, w, "window", "https://github.com/eval/repo", root, chunk.StrategyWindow, true)

	// The same paths, one file's bytes changed — the failure mode is a harness
	// pointed at the right paths at the wrong commit, and paths are identical
	// across commits.
	_, blobs := cases(t, root)
	blobs["store.go"] = store.BlobHash([]byte("package repo\n"))
	_, err := Verify(context.Background(), ast, win, blobs)
	if !errors.Is(err, ErrSourceMismatch) {
		t.Errorf("Verify gave %v; want ErrSourceMismatch naming store.go", err)
	}
	if err != nil && !strings.Contains(err.Error(), "store.go") {
		t.Errorf("the refusal is %q; it must name the path", err)
	}
}

// The test the spec's premise depends on: a test that fails if stripping
// regresses, rather than a comment claiming it holds.
func TestAnUnstrippedCorpusIsRefusedByTheProbeLive(t *testing.T) {
	a, _ := dsns(t)
	// NOTES.md is left out so the only leak the probe can find is the one
	// stripping was supposed to remove.
	root := checkout(t, fixtureRoot, "NOTES.md")
	cs, _ := cases(t, root)
	if len(cs) == 0 {
		t.Fatal("the fixture generated no cases")
	}

	stripped := build(t, a, "stripped", "https://github.com/eval/stripped", root, chunk.StrategyAST, true)
	leaks, err := Probe(context.Background(), stripped, cs)
	if err != nil || leaks != nil {
		t.Fatalf("the stripped corpus leaked %+v: %v", leaks, err)
	}

	raw := build(t, a, "unstripped", "https://github.com/eval/unstripped", root, chunk.StrategyAST, false)
	leaks, err = Probe(context.Background(), raw, cs)
	if !errors.Is(err, ErrLeaked) {
		t.Fatalf("the unstripped corpus gave %v, want ErrLeaked", err)
	}
	// Naming a case and a span: "returns an error" would pass under a probe
	// that errored for any reason at all.
	if len(leaks) == 0 {
		t.Fatal("ErrLeaked with no leak named")
	}
	want := "store.go:Store.Get:decl"
	var found bool
	for _, l := range leaks {
		if l.CaseID == want {
			found = true
			if l.Path != "store.go" || l.SpanID == "" {
				t.Errorf("leak for %s is %+v", want, l)
			}
		}
	}
	if !found {
		t.Errorf("probe reported %+v; want one naming case %q", leaks, want)
	}
	if !strings.Contains(err.Error(), "was not stripped") {
		t.Errorf("the refusal is %q; it has to say what is wrong with the corpus", err)
	}
	t.Logf("unstripped corpus leaked %d of %d cases", len(leaks), len(cs))
}

// The leak class stripping cannot close: StripDocs passes a non-Go file
// through unchanged, so a repository whose markdown repeats a doc comment
// carries that prose into both arms.
func TestTheProbeFindsProseInANonGoFileLive(t *testing.T) {
	a, _ := dsns(t)
	root := checkout(t, fixtureRoot)
	cs, _ := cases(t, root)
	arm := build(t, a, "ast", "https://github.com/eval/notes", root, chunk.StrategyAST, true)

	leaks, err := Probe(context.Background(), arm, cs)
	if !errors.Is(err, ErrLeaked) {
		t.Fatalf("Probe gave %v on a stripped corpus holding NOTES.md; want ErrLeaked", err)
	}
	for _, l := range leaks {
		if l.Path != "NOTES.md" {
			t.Errorf("leak at %s; the Go side of this corpus is stripped and must be clean: %+v", l.Path, l)
		}
	}
	if len(leaks) != 1 || leaks[0].CaseID != "store.go:Store.Get:decl" {
		t.Errorf("probe found %+v; want exactly one leak, case store.go:Store.Get:decl at NOTES.md", leaks)
	}
}

func TestTheProbeIsScopedToOneRepoLive(t *testing.T) {
	a, _ := dsns(t)
	root := checkout(t, fixtureRoot, "NOTES.md")
	cs, _ := cases(t, root)

	// A second repository in the same database whose span text is one golden
	// question's prose verbatim. It survives stripping because the prose is
	// inside a function body, which StripDocs keeps.
	decoy := build(t, a, "decoy", "https://github.com/eval/decoy", checkout(t, decoyRoot), chunk.StrategyAST, true)
	if leaks, err := Probe(context.Background(), decoy, cs); !errors.Is(err, ErrLeaked) || len(leaks) == 0 {
		t.Fatalf("the decoy repository does not hold the prose: %+v / %v; the fixture cannot discriminate", leaks, err)
	}

	target := build(t, a, "ast", "https://github.com/eval/scoped", root, chunk.StrategyAST, true)
	if leaks, err := Probe(context.Background(), target, cs); err != nil || leaks != nil {
		t.Errorf("probing the target reported %+v / %v; the leak is in another repository", leaks, err)
	}
}
