package corpus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
	"github.com/mralaminahamed/codetrail/packages/shared/testdb"
)

// fake is an in-memory Reader, which is what the interface exists for: a
// concrete *store.Store cannot be faked, so these checks would otherwise only
// be testable against a live database.
type fake struct {
	commit string
	blobs  map[string]string
	spans  []metric.Span
	texts  []TextRow
	// pageSize, when set, overrides the caller's limit — the fixture for a
	// pager that has to advance.
	pageSize int
	err      map[string]error
}

func (f *fake) fail(op string) error {
	if f.err == nil {
		return nil
	}
	return f.err[op]
}

func (f *fake) Commit(_ context.Context, _ string) (string, error) {
	return f.commit, f.fail("Commit")
}

func (f *fake) FileBlobs(_ context.Context, _ string) (map[string]string, error) {
	if err := f.fail("FileBlobs"); err != nil {
		return nil, err
	}
	return f.blobs, nil
}

func (f *fake) SpanRanges(_ context.Context, _ string) ([]metric.Span, error) {
	if err := f.fail("SpanRanges"); err != nil {
		return nil, err
	}
	return f.spans, nil
}

func (f *fake) SpanTexts(_ context.Context, _, after string, limit int) ([]TextRow, string, error) {
	if err := f.fail("SpanTexts"); err != nil {
		return nil, "", err
	}
	if f.pageSize > 0 {
		limit = f.pageSize
	}
	var out []TextRow
	for _, t := range f.texts {
		if t.SpanID > after {
			out = append(out, t)
		}
		if len(out) == limit {
			return out, out[len(out)-1].SpanID, nil
		}
	}
	return out, "", nil
}

func arm(name, db string, f *fake) Arm {
	return Arm{Name: name, Database: db, RepoID: "r", Read: f}
}

func spansAt(ids ...string) []metric.Span {
	out := make([]metric.Span, 0, len(ids))
	for i, id := range ids {
		out = append(out, metric.Span{ID: id, Path: "store.go", Start: i*10 + 1, End: i*10 + 9})
	}
	return out
}

func base() (*fake, *fake, map[string]string) {
	blobs := map[string]string{"store.go": "aaa", "NOTES.md": "bbb"}
	a := &fake{commit: "c0ffee", blobs: blobs, spans: spansAt("a1", "a2")}
	b := &fake{commit: "c0ffee", blobs: blobs, spans: spansAt("b1", "b2")}
	src := map[string]string{"store.go": "aaa", "NOTES.md": "bbb"}
	return a, b, src
}

func TestTwoDsnsNamingOneDatabaseAreRefused(t *testing.T) {
	a, b, src := base()
	// Two DSNs differing only in a query parameter. Textually identical DSNs
	// cannot discriminate, which is what a string comparison passes on.
	one := "postgres://u:p@h:5432/codetrail_eval?sslmode=disable"
	two := "postgres://u:p@h:5432/codetrail_eval?pool_max_conns=4"
	if testdb.Name(one) != testdb.Name(two) {
		t.Fatalf("the fixture's two DSNs name %q and %q", testdb.Name(one), testdb.Name(two))
	}
	if one == two {
		t.Fatal("the fixture's two DSNs are textually identical and cannot discriminate")
	}
	_, err := Verify(context.Background(), arm("ast", testdb.Name(one), a), arm("window", testdb.Name(two), b), src)
	if !errors.Is(err, ErrSameDatabase) {
		t.Errorf("Verify accepted two DSNs naming database %s: %v; want ErrSameDatabase", testdb.Name(one), err)
	}
}

func TestArmsAtDifferentCommitsAreRefused(t *testing.T) {
	a, b, src := base()
	b.commit = "deadbee"
	_, err := Verify(context.Background(), arm("ast", "ast_db", a), arm("window", "win_db", b), src)
	if !errors.Is(err, ErrCommitMismatch) {
		t.Errorf("Verify gave %v, want ErrCommitMismatch", err)
	}
	if err != nil && (!strings.Contains(err.Error(), "c0ffee") || !strings.Contains(err.Error(), "deadbee")) {
		t.Errorf("the refusal is %q; it must name both commits", err)
	}
}

func TestArmsWithDifferentFileBytesAreRefusedEvenAtOneCommit(t *testing.T) {
	a, b, src := base()
	// The same file count and one differing blob: a fixture where the arms
	// differ in file count cannot tell a count check from a byte check.
	b.blobs = map[string]string{"store.go": "ZZZ", "NOTES.md": "bbb"}
	if len(a.blobs) != len(b.blobs) {
		t.Fatalf("the fixture's file counts differ (%d and %d) and cannot discriminate", len(a.blobs), len(b.blobs))
	}
	_, err := Verify(context.Background(), arm("ast", "ast_db", a), arm("window", "win_db", b), src)
	if !errors.Is(err, ErrFileSetMismatch) {
		t.Errorf("Verify gave %v; want ErrFileSetMismatch naming store.go", err)
	}
	if err != nil && !strings.Contains(err.Error(), "store.go") {
		t.Errorf("the refusal is %q; it must name the path", err)
	}
}

func TestArmsWithIdenticalSpanSetsAreRefused(t *testing.T) {
	a, b, src := base()
	b.spans = a.spans
	_, err := Verify(context.Background(), arm("ast", "ast_db", a), arm("window", "win_db", b), src)
	if !errors.Is(err, ErrIdenticalArms) {
		t.Errorf("Verify accepted two arms with identical span ids: %v; want ErrIdenticalArms", err)
	}
	// Equal counts with disjoint ids must pass — P2's real numbers differ
	// (1,303 against 768), so the realistic fixture is the one a count check
	// gets right by accident.
	b.spans = spansAt("b1", "b2")
	if _, err := Verify(context.Background(), arm("ast", "ast_db", a), arm("window", "win_db", b), src); err != nil {
		t.Errorf("two arms with equal counts and disjoint ids gave %v, want nil", err)
	}
}

func TestSharedSpanIdsAreReportedAndNotRefused(t *testing.T) {
	a, b, src := base()
	b.spans = spansAt("a1", "b2", "b3")
	rep, err := Verify(context.Background(), arm("ast", "ast_db", a), arm("window", "win_db", b), src)
	if err != nil {
		t.Fatalf("Verify gave %v; a nonzero intersection is normal", err)
	}
	if rep.SharedSpanIDs != 1 {
		t.Errorf("SharedSpanIDs is %d, want 1", rep.SharedSpanIDs)
	}
	if rep.Commit != "c0ffee" || rep.Spans["ast"] != 2 || rep.Spans["window"] != 3 || rep.Files["ast"] != 2 {
		t.Errorf("report is %+v", rep)
	}
}

func TestSourceBytesThatNoArmIndexedAreRefused(t *testing.T) {
	a, b, src := base()
	src["extra.go"] = "ccc"
	_, err := Verify(context.Background(), arm("ast", "ast_db", a), arm("window", "win_db", b), src)
	if !errors.Is(err, ErrSourceMismatch) {
		t.Errorf("Verify gave %v; want ErrSourceMismatch naming extra.go", err)
	}
	if err != nil && !strings.Contains(err.Error(), "extra.go") {
		t.Errorf("the refusal is %q; it must name the path", err)
	}
}

func TestSourceBytesThatDisagreeWithAnArmsBlobAreRefused(t *testing.T) {
	a, b, src := base()
	// The same paths as the corpus, one file's bytes changed: a fixture with a
	// missing or extra file cannot tell a set check from a byte check, and
	// paths are identical across commits, which is the failure mode.
	src["store.go"] = "different"
	if len(src) != len(a.blobs) {
		t.Fatalf("the fixture's path sets differ and cannot discriminate")
	}
	_, err := Verify(context.Background(), arm("ast", "ast_db", a), arm("window", "win_db", b), src)
	if !errors.Is(err, ErrSourceMismatch) {
		t.Errorf("Verify gave %v; want ErrSourceMismatch naming store.go", err)
	}
}

func TestVerifyReportsTheMostSpecificFailureFirst(t *testing.T) {
	// One database and identical arms at once: two DSNs naming one database
	// always produce identical span sets, so a looser order sends an operator
	// to fix the wrong thing.
	a, _, src := base()
	b := &fake{commit: a.commit, blobs: a.blobs, spans: a.spans}
	_, err := Verify(context.Background(), arm("ast", "one_db", a), arm("window", "one_db", b), src)
	if !errors.Is(err, ErrSameDatabase) {
		t.Errorf("Verify gave %v, want ErrSameDatabase: it is the more specific of the two rules this fixture breaks", err)
	}
}

func TestEachReadsFailureIsReportedAndNamed(t *testing.T) {
	// One fake with four methods is four error branches, and wiring one proves
	// nothing about the other three.
	for _, op := range []string{"Commit", "FileBlobs", "SpanRanges"} {
		a, b, src := base()
		boom := errors.New("boom")
		a.err = map[string]error{op: boom}
		_, err := Verify(context.Background(), arm("ast", "ast_db", a), arm("window", "win_db", b), src)
		if !errors.Is(err, boom) {
			t.Errorf("%s failing gave %v, want the underlying error", op, err)
		}
		if err != nil && !strings.Contains(err.Error(), "ast") {
			t.Errorf("%s failing gave %q; it must name the arm", op, err)
		}
	}
	a, _, _ := base()
	a.err = map[string]error{"SpanTexts": errors.New("boom")}
	if _, err := Probe(context.Background(), arm("ast", "ast_db", a), nil); err == nil || !strings.Contains(err.Error(), "ast") {
		t.Errorf("SpanTexts failing gave %v; want an error naming the arm", err)
	}
}

// The leak fixtures. store.go is stripped and clean; NOTES.md is not Go, so
// StripDocs passed it through and it carries one symbol's prose verbatim.
func leakCorpus() (*fake, []golden.Case) {
	cases := []golden.Case{
		{ID: "store.go:Store.Get:decl", Question: "Get returns the thing named n.\nIt returns an error if n is unknown.\n"},
		{ID: "store.go:Store.Put:decl", Question: "Put writes v under k.\n"},
	}
	f := &fake{texts: []TextRow{
		{SpanID: "s1", Path: "store.go", Text: "func (s *Store) Get(n string) (int, error) { return 0, nil }"},
		{SpanID: "s2", Path: "store.go", Text: "func (s *Store) Put(k string, v int) {}"},
		{SpanID: "s3", Path: "NOTES.md", Text: "# Notes\n\nGet returns the thing named n.\nIt returns an error if n is unknown.\n"},
	}}
	return f, cases
}

func TestTheProbeFindsProseInANonGoFile(t *testing.T) {
	f, cases := leakCorpus()
	leaks, err := Probe(context.Background(), arm("ast", "ast_db", f), cases)
	if !errors.Is(err, ErrLeaked) {
		t.Fatalf("Probe gave %v, want ErrLeaked", err)
	}
	if len(leaks) != 1 {
		t.Fatalf("probe reported %d leaks %+v; want 1 naming case %q in a span at NOTES.md", len(leaks), leaks, cases[0].ID)
	}
	// The path, not the count: it is what says whether the leak is a missed
	// .go file or a README.md.
	if leaks[0] != (Leak{CaseID: "store.go:Store.Get:decl", SpanID: "s3", Path: "NOTES.md"}) {
		t.Errorf("leak is %+v", leaks[0])
	}
	if !strings.Contains(err.Error(), "NOTES.md") || !strings.Contains(err.Error(), cases[0].ID) {
		t.Errorf("the refusal is %q; it must name the case and the path", err)
	}
}

func TestTheProbeFindsAMultiLineDocCommentInUnstrippedSpanText(t *testing.T) {
	// An unstripped corpus: the doc comment is inside its own span's text,
	// with the "// " that Doc.Text() removed. A one-line fixture cannot
	// discriminate — the prose survives verbatim inside "// …" either way.
	f, cases := leakCorpus()
	f.texts = []TextRow{
		{SpanID: "s1", Path: "store.go", Text: "// Get returns the thing named n.\n// It returns an error if n is unknown.\nfunc (s *Store) Get(n string) (int, error) { return 0, nil }"},
		{SpanID: "s2", Path: "store.go", Text: "// Put writes v under k.\nfunc (s *Store) Put(k string, v int) {}"},
	}
	leaks, err := Probe(context.Background(), arm("ast", "ast_db", f), cases)
	if !errors.Is(err, ErrLeaked) {
		t.Fatalf("Probe gave %v, want ErrLeaked", err)
	}
	if len(leaks) != 2 {
		t.Errorf("probe found %d leaks %+v on an unstripped corpus, want 2 (the two-line comment is the one exact containment misses)", len(leaks), leaks)
	}
}

func TestTheProbeScansEverySpanAndNotOnlyTheGoldOne(t *testing.T) {
	f, cases := leakCorpus()
	// The leaking span is not any case's answer: it is a test file quoting the
	// doc comment. A probe that checked only the gold span reports a clean
	// corpus.
	f.texts = append(f.texts[:2], TextRow{SpanID: "s9", Path: "store_test.go",
		Text: "// Put writes v under k. -- copied from the doc comment\nfunc TestPut(t *testing.T) {}"})
	leaks, err := Probe(context.Background(), arm("ast", "ast_db", f), cases)
	if !errors.Is(err, ErrLeaked) || len(leaks) != 1 || leaks[0].Path != "store_test.go" {
		t.Errorf("probe reported %+v / %v; want one leak at store_test.go", leaks, err)
	}
}

func TestTheProbeFindsALeakBeyondTheFirstPage(t *testing.T) {
	f, cases := leakCorpus()
	// Seven spans, a page size of two, and the leak in the last page. A
	// fixture that fits in one page cannot tell a working cursor from a pager
	// that returns the first page forever.
	f.texts = []TextRow{
		{SpanID: "s1", Path: "a.go", Text: "func A() {}"},
		{SpanID: "s2", Path: "a.go", Text: "func B() {}"},
		{SpanID: "s3", Path: "a.go", Text: "func C() {}"},
		{SpanID: "s4", Path: "a.go", Text: "func D() {}"},
		{SpanID: "s5", Path: "a.go", Text: "func E() {}"},
		{SpanID: "s6", Path: "a.go", Text: "func F() {}"},
		{SpanID: "s7", Path: "NOTES.md", Text: "Put writes v under k."},
	}
	f.pageSize = 2
	leaks, err := Probe(context.Background(), arm("ast", "ast_db", f), cases)
	if !errors.Is(err, ErrLeaked) || len(leaks) != 1 || leaks[0].Path != "NOTES.md" {
		t.Errorf("probe found %+v / %v with a page size of 2 over 7 spans; want 1 leak at NOTES.md", leaks, err)
	}
}

func TestTheProbeIsScopedToOneRepo(t *testing.T) {
	// The Reader is repo-scoped by construction: Probe passes a.RepoID to
	// every read and never a second one. The fixture asserts the id that
	// reaches the reader rather than trusting the signature.
	var got []string
	f, cases := leakCorpus()
	f.texts = nil
	spy := &spyReader{fake: f, seen: &got}
	if _, err := Probe(context.Background(), Arm{Name: "ast", Database: "d", RepoID: "repo-A", Read: spy}, cases); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the probe issued no read at all")
	}
	for _, id := range got {
		if id != "repo-A" {
			t.Errorf("the probe read repo %q, want repo-A", id)
		}
	}
}

type spyReader struct {
	*fake
	seen *[]string
}

func (s *spyReader) SpanTexts(ctx context.Context, repoID, after string, limit int) ([]TextRow, string, error) {
	*s.seen = append(*s.seen, repoID)
	return s.fake.SpanTexts(ctx, repoID, after, limit)
}

func TestACleanCorpusProbesClean(t *testing.T) {
	f, cases := leakCorpus()
	f.texts = f.texts[:2]
	leaks, err := Probe(context.Background(), arm("ast", "ast_db", f), cases)
	if err != nil || leaks != nil {
		t.Errorf("a stripped corpus gave %+v / %v, want no leaks and no error", leaks, err)
	}
}
