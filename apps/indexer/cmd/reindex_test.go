package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/clone"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// embedCounter counts the TEXTS handed to the embedder, which is the only
// observer the reuse pass has: the rows are identical whether a vector was
// reused or recomputed, so the mutation is invisible in the output and visible
// only in the work.
//
// embedCounter wraps whatever embedder fakeIndexer built and counts.
type embedCounter struct {
	inner interface {
		Model() string
		Dim() int
		Embed(ctx context.Context, texts []string) ([][]float32, error)
	}
	n *int
}

func (e embedCounter) Model() string { return e.inner.Model() }
func (e embedCounter) Dim() int      { return e.inner.Dim() }

func (e embedCounter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	*e.n += len(texts)
	return e.inner.Embed(ctx, texts)
}

// counted returns an indexer whose embedder counts texts.
func counted(t *testing.T, opts ...fixtureOpt) (*indexer, *recorder, *int) {
	t.Helper()
	ix, rec := fakeIndexer(t, opts...)
	n := 0
	ix.emb = embedCounter{inner: ix.emb, n: &n}
	return ix, rec, &n
}

// prime makes every span's digest already known to the store, as though an
// earlier commit had embedded them.
//
// IT ANSWERS ONLY FOR KEYS THAT ARE DIGESTS, and that is load-bearing rather
// than fussy. store.Digest is sha256 at 64 hex characters; store.SpanID is
// sha256 truncated to 16 bytes, 32 hex. A stub that echoed back whatever key it
// was handed could not tell the two apart — measured: the mutation that keys
// reuse on the span id SURVIVED against the echoing version, because the stub
// obligingly answered for span ids too.
func prime(ix *indexer, vec []float32) {
	ix.embeddings = func(_ context.Context, keys []string, _ string, _ int) (map[string][]float32, error) {
		out := make(map[string][]float32, len(keys))
		for _, k := range keys {
			if isDigest(k) {
				out[k] = vec
			}
		}
		return out, nil
	}
}

// isDigest is the shape store.Digest produces: 64 lowercase hex. A span id is
// half that.
func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func vecOf(ix *indexer) []float32 {
	v := make([]float32, ix.emb.Dim())
	v[0] = 1
	return v
}

// ---- the embedding reuse --------------------------------------------------

func TestAnUnchangedDeclarationIsNotReEmbedded(t *testing.T) {
	ix, rec, n := counted(t)
	prime(ix, vecOf(ix))
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	if *n != 0 {
		t.Errorf("the embedder was called for %d texts, want 0: every digest was already known", *n)
	}
	if len(rec.spans) == 0 {
		t.Fatal("the job wrote no spans, so this test measures nothing")
	}
	// Every span still carries a vector, so "reuse" is not "skip".
	for _, sp := range rec.spans {
		if len(sp.Embedding) != ix.emb.Dim() {
			t.Fatalf("span %s has a %d-component vector, want %d", sp.ID, len(sp.Embedding), ix.emb.Dim())
		}
	}
}

func TestAChangedDeclarationIsReEmbedded(t *testing.T) {
	ix, rec, n := counted(t)
	// Only ONE digest is known. Everything else has to be embedded, so this
	// separates "reuse hits what it can" from "reuse hits everything" and from
	// "reuse hits nothing".
	var known string
	ix.embeddings = func(_ context.Context, digests []string, _ string, _ int) (map[string][]float32, error) {
		if len(digests) == 0 {
			return nil, nil
		}
		known = digests[0]
		return map[string][]float32{known: vecOf(ix)}, nil
	}
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	reusable := 0
	for _, sp := range rec.spans {
		if sp.Digest == known {
			reusable++
		}
	}
	if want := len(rec.spans) - reusable; *n != want {
		t.Errorf("the embedder was called for %d texts, want %d (%d spans, %d reusable)",
			*n, want, len(rec.spans), reusable)
	}
	if *n == 0 || *n == len(rec.spans) {
		t.Errorf("the fixture cannot separate partial reuse from none or all: %d of %d", *n, len(rec.spans))
	}
}

// Fixture rule 8: two spans with IDENTICAL text inside one commit — a repeated
// helper — so a map[digest] that assumes one row per digest silently drops one.
// A corpus of distinct spans cannot fire, and this is the most likely real
// defect in the task.
func TestTwoIdenticalSpansInOneCommitBothGetTheirVector(t *testing.T) {
	const helper = "package p\n\nfunc Helper() int { return 1 }\n"
	ix, rec, n := counted(t, withFiles(map[string]string{
		"one/util.go": helper,
		"two/util.go": helper,
	}))
	prime(ix, vecOf(ix))
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	// The fixture is proved able to fire: two spans really do share one digest.
	byDigest := map[string]int{}
	for _, sp := range rec.spans {
		byDigest[sp.Digest]++
	}
	shared := 0
	for _, c := range byDigest {
		if c > 1 {
			shared++
		}
	}
	if shared == 0 {
		t.Fatalf("no two spans share a digest; this fixture cannot detect a consumed map: %v", rec.spansByPath())
	}
	for _, sp := range rec.spans {
		if len(sp.Embedding) != ix.emb.Dim() {
			t.Errorf("span %s (%s:%d-%d) has a nil embedding: a map[digest] is a lookup, not a consumption",
				sp.ID, sp.Path, sp.StartLine, sp.EndLine)
		}
	}
	if *n != 0 {
		t.Errorf("the embedder was called for %d texts, want 0", *n)
	}
}

// A stub store that lends a vector of the WRONG WIDTH.
//
// This replaces the digest re-check the plan asked for, which the mutation
// round showed to be unreachable: the lookup is keyed on the digest, so a Go
// map already guarantees the key, and store.Digest(sp.Text) == sp.Digest is true
// by construction. Deleting that check left every test green.
//
// The width can differ and does defend something: spans.embed_dim has no CHECK
// tying it to the vector column's width, so a corrupt row exists in principle,
// and lending from it would put two vector spaces in one corpus.
func TestAReusedVectorOfTheWrongWidthIsRefused(t *testing.T) {
	ix, rec, n := counted(t)
	ix.embeddings = func(_ context.Context, digests []string, _ string, _ int) (map[string][]float32, error) {
		out := map[string][]float32{}
		for _, d := range digests {
			out[d] = make([]float32, ix.emb.Dim()/2)
		}
		return out, nil
	}
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	// Every span was embedded rather than borrowed.
	if *n != len(rec.spans) {
		t.Errorf("the embedder was called for %d texts, want %d: a wrong-width vector must not be lent",
			*n, len(rec.spans))
	}
	for _, sp := range rec.spans {
		if len(sp.Embedding) != ix.emb.Dim() {
			t.Errorf("span %s holds a %d-component vector, want %d", sp.ID, len(sp.Embedding), ix.emb.Dim())
		}
	}
	if !strings.Contains(rec.logged.String(), "wrong width") {
		t.Errorf("the refusal was not logged: %s", rec.logged.String())
	}
}

// A map answering under a key nobody asked with simply misses, and every span is
// embedded. Asserted so the behaviour is recorded rather than assumed — it is
// what makes the digest re-check unnecessary.
func TestAWrongKeyedReuseMapSimplyMisses(t *testing.T) {
	ix, rec, n := counted(t)
	ix.embeddings = func(_ context.Context, digests []string, _ string, _ int) (map[string][]float32, error) {
		out := map[string][]float32{}
		for _, d := range digests {
			out[d+"-wrong"] = vecOf(ix)
		}
		return out, nil
	}
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	if *n != len(rec.spans) {
		t.Errorf("the embedder was called for %d texts, want %d", *n, len(rec.spans))
	}
}

// Reuse is an optimisation, and an optimisation that can fail a job is a new
// failure mode for every job.
func TestAFailedReuseReadEmbedsEverythingAndDoesNotFailTheJob(t *testing.T) {
	ix, rec, n := counted(t)
	ix.embeddings = func(context.Context, []string, string, int) (map[string][]float32, error) {
		return nil, errors.New("pool exhausted")
	}
	ix.runJob(context.Background(), aJob())
	if got := rec.failReason(); got != "" {
		t.Errorf("the job failed on a reuse read: %s", got)
	}
	if len(rec.spans) == 0 {
		t.Fatal("the job wrote no spans")
	}
	if *n != len(rec.spans) {
		t.Errorf("the embedder was called for %d texts, want %d", *n, len(rec.spans))
	}
	if !strings.Contains(rec.logged.String(), "the reuse read failed") {
		t.Errorf("the failure was not logged: %s", rec.logged.String())
	}
}

// embedAll is handed only the spans reuse did not fill, asserted by CALL COUNT
// rather than by rows: the rows are identical either way.
func TestEmbedAllIsHandedOnlyTheSpansReuseDidNotFill(t *testing.T) {
	ix, rec, n := counted(t)
	prime(ix, vecOf(ix))
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	if *n != 0 {
		t.Errorf("the embedder was called for %d texts, want 0", *n)
	}
	// And the counterfactual, so the zero above is a property of the reuse and
	// not of a fixture that produces no spans.
	ix2, rec2, n2 := counted(t)
	ix2.runJob(context.Background(), aJob())
	if *n2 != len(rec2.spans) || *n2 == 0 {
		t.Fatalf("without reuse the embedder was called for %d texts and %d spans were written",
			*n2, len(rec2.spans))
	}
}

// The house rule, and the values that separate a parse from a comparison.
func TestReuseKnobsAreParsedNotCompared(t *testing.T) {
	for _, v := range []string{"TRUE", "1", "t", "True"} {
		t.Setenv("REINDEX_REUSE", v)
		t.Setenv("REINDEX_SKIP_CLONE", v)
		skip, reuse, err := reindexKnobs()
		if err != nil {
			t.Fatalf("REINDEX_REUSE=%s: %v", v, err)
		}
		if !reuse || !skip {
			t.Errorf("REINDEX_REUSE=%s parsed as reuse=%v skip=%v, want both true", v, reuse, skip)
		}
	}
	for _, v := range []string{"FALSE", "0", "f"} {
		t.Setenv("REINDEX_REUSE", v)
		t.Setenv("REINDEX_SKIP_CLONE", v)
		skip, reuse, err := reindexKnobs()
		if err != nil || reuse || skip {
			t.Errorf("REINDEX_REUSE=%s parsed as reuse=%v skip=%v err=%v, want both false", v, reuse, skip, err)
		}
	}
	t.Setenv("REINDEX_REUSE", "yes please")
	if _, _, err := reindexKnobs(); err == nil {
		t.Errorf("REINDEX_REUSE=%q was accepted", "yes please")
	} else if !strings.Contains(err.Error(), "REINDEX_REUSE") {
		t.Errorf("the error does not name the knob: %v", err)
	}
}

// REINDEX_REUSE=false is what makes the counterfactual runnable at all.
func TestReuseOffEmbedsEverything(t *testing.T) {
	t.Setenv("REINDEX_REUSE", "false")
	ix, rec, n := counted(t)
	lim, err := limitsFrom()
	if err != nil {
		t.Fatal(err)
	}
	ix.lim.reuse = lim.reuse
	prime(ix, vecOf(ix))
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	if *n != len(rec.spans) {
		t.Errorf("with REINDEX_REUSE=false the embedder was called for %d texts, want %d", *n, len(rec.spans))
	}
}

// ---- the fast path --------------------------------------------------------

// fastPathIndexer turns REINDEX_SKIP_CLONE on and gives resolve an answer.
func fastPathIndexer(t *testing.T, sha string, opts ...fixtureOpt) (*indexer, *recorder) {
	t.Helper()
	ix, rec := fakeIndexer(t, opts...)
	ix.lim.skipClone = true
	ix.resolve = func(context.Context, string, string, clone.Limits) (string, error) { return sha, nil }
	return ix, rec
}

// ONE function computes the identity for both paths, and this asserts the two
// agree. Asserting the fast path's id alone cannot fire — it would be
// consistently wrong and consistently self-agreeing, which is exactly how the
// P1 blocker shipped.
func TestBothPathsComputeTheSameRepoIdForOneCommit(t *testing.T) {
	// The cloning path, whose clone stub returns testCommit.
	slow, slowRec := fakeIndexer(t)
	slow.runJob(context.Background(), aJob())
	if slowRec.failReason() != "" {
		t.Fatalf("the cloning job failed: %s", slowRec.failReason())
	}
	if len(slowRec.spans) == 0 {
		t.Fatal("the cloning job wrote no spans")
	}
	if len(slowRec.repoIDs) != 1 {
		t.Fatalf("the cloning job recorded %d repo ids, want 1", len(slowRec.repoIDs))
	}
	slowID := slowRec.repoIDs[0]
	if slowID != slowRec.spans[0].RepoID {
		t.Fatalf("the completed repo id %s is not the one the spans were written under (%s)",
			slowID, slowRec.spans[0].RepoID)
	}

	// The fast path, whose resolve returns the same sha.
	fast, fastRec := fastPathIndexer(t, testCommit)
	fast.getRepo = func(_ context.Context, id string) (models.Repo, error) {
		return models.Repo{ID: id}, nil
	}
	fast.countSpans = func(context.Context, string) (int, error) { return 4, nil }
	fast.spanEmbedder = func(context.Context, string) (string, int, error) {
		return fast.emb.Model(), fast.emb.Dim(), nil
	}
	fast.runJob(context.Background(), aJob())
	if len(fastRec.repoIDs) != 1 {
		t.Fatalf("the fast path completed %d jobs, want 1", len(fastRec.repoIDs))
	}
	fastID := fastRec.repoIDs[0]

	if fastID != slowID {
		t.Errorf("fast path id %s, clone path id %s", fastID, slowID)
	}
}

func TestAnAlreadyIndexedCommitCompletesWithoutCloning(t *testing.T) {
	ix, rec := fastPathIndexer(t, testCommit)
	ix.getRepo = func(_ context.Context, id string) (models.Repo, error) { return models.Repo{ID: id}, nil }
	ix.countSpans = func(context.Context, string) (int, error) { return 4, nil }
	ix.spanEmbedder = func(context.Context, string) (string, int, error) {
		return ix.emb.Model(), ix.emb.Dim(), nil
	}
	// clone, walk, put and putSpans all fail the test if they run — testIndexer
	// wires them that way — so "it did not clone" needs no assertion of its own.
	ix.runJob(context.Background(), aJob())

	if len(rec.completed) != 1 {
		t.Fatalf("completed %d jobs, want 1", len(rec.completed))
	}
	if rec.failReason() != "" {
		t.Errorf("the job failed: %s", rec.failReason())
	}
	if !strings.Contains(rec.logged.String(), "completing without cloning") {
		t.Errorf("the skip was not logged: %s", rec.logged.String())
	}
}

// A repo row with zero spans is a completed job with an empty index, and
// serving it as a cache hit makes the emptiness PERMANENT — a repository that
// can never be re-indexed by re-submitting it.
func TestARepoRowWithNoSpansIsNotAHit(t *testing.T) {
	ix, rec := fastPathIndexer(t, testCommit)
	ix.getRepo = func(_ context.Context, id string) (models.Repo, error) { return models.Repo{ID: id}, nil }
	ix.countSpans = func(context.Context, string) (int, error) { return 0, nil }
	ix.spanEmbedder = func(context.Context, string) (string, int, error) {
		t.Error("the embedder check ran although the span count was already zero")
		return "", 0, nil
	}
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	// It cloned, so the corpus is rebuilt rather than the emptiness being served.
	if len(rec.spans) == 0 {
		t.Errorf("the job completed done with 0 spans and never cloned")
	}
}

// RepoID contains no model and no dimension, so nothing else in the system
// stops a re-index after an EMBED_MODEL change from being a no-op — and with
// REINDEX_SKIP_CLONE defaulting to true, that would be the default behaviour.
func TestChangingTheEmbedderDefeatsTheFastPath(t *testing.T) {
	ix, rec := fastPathIndexer(t, testCommit)
	ix.getRepo = func(_ context.Context, id string) (models.Repo, error) { return models.Repo{ID: id}, nil }
	ix.countSpans = func(context.Context, string) (int, error) { return 4, nil }
	// The corpus was embedded by another model.
	ix.spanEmbedder = func(context.Context, string) (string, int, error) {
		return "some-other-model", ix.emb.Dim(), nil
	}
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	// It cloned AND re-embedded, and the spans now carry this worker's model —
	// not merely that the job completed, which it does under both.
	if len(rec.spans) == 0 {
		t.Fatalf("the job completed done with spans still at embed_model %q", "some-other-model")
	}
	if rec.model != ix.emb.Model() {
		t.Errorf("spans written under embed_model %q, want %q", rec.model, ix.emb.Model())
	}

	// The same for the width, which is the other half of the same invariant.
	ix2, rec2 := fastPathIndexer(t, testCommit)
	ix2.getRepo = func(_ context.Context, id string) (models.Repo, error) { return models.Repo{ID: id}, nil }
	ix2.countSpans = func(context.Context, string) (int, error) { return 4, nil }
	ix2.spanEmbedder = func(context.Context, string) (string, int, error) {
		return ix2.emb.Model(), ix2.emb.Dim() / 2, nil
	}
	ix2.runJob(context.Background(), aJob())
	if len(rec2.spans) == 0 {
		t.Errorf("a corpus of another width was served as a cache hit")
	}
}

// An optimisation that can fail a job is a new failure mode for every job.
func TestAnUnresolvableRefFallsBackToCloningRatherThanFailing(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.lim.skipClone = true
	ix.resolve = func(context.Context, string, string, clone.Limits) (string, error) {
		return "", errors.New("ls-remote: exit status 128")
	}
	ix.getRepo = func(context.Context, string) (models.Repo, error) {
		t.Error("the fast path read a repo row although the ref did not resolve")
		return models.Repo{}, nil
	}
	ix.runJob(context.Background(), aJob())
	// The job's STATUS and its row count, not a log line.
	if got := rec.failReason(); got != "" {
		t.Errorf("job failed with %q, want done", got)
	}
	if len(rec.completed) != 1 {
		t.Errorf("completed %d jobs, want 1", len(rec.completed))
	}
	if len(rec.spans) == 0 {
		t.Errorf("the job completed with no spans, so it never cloned either")
	}
}

func TestSkipCloneFalseAlwaysClones(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.lim.skipClone = false
	// resolve fails the test if it runs, from testIndexer.
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	if len(rec.spans) == 0 {
		t.Errorf("the job wrote no spans")
	}
}

// PutRepo adds and updates and never deletes, so a run producing a smaller file
// set leaves the first run's rows behind and file_count disagrees with the row
// count. The fast path has no file list at all, so it would write file_count=0
// over a correct value.
func TestTheFastPathWritesNothingToReposFilesOrSpans(t *testing.T) {
	ix, rec := fastPathIndexer(t, testCommit)
	ix.getRepo = func(_ context.Context, id string) (models.Repo, error) { return models.Repo{ID: id}, nil }
	ix.countSpans = func(context.Context, string) (int, error) { return 4, nil }
	ix.spanEmbedder = func(context.Context, string) (string, int, error) {
		return ix.emb.Model(), ix.emb.Dim(), nil
	}
	ix.runJob(context.Background(), aJob())
	// put and putSpans fail the test if they run, so this asserts the recorder
	// stayed empty as well.
	if len(rec.files) != 0 || len(rec.spans) != 0 {
		t.Errorf("the fast path wrote %d files and %d spans, want none", len(rec.files), len(rec.spans))
	}
	if len(rec.order) != 0 {
		t.Errorf("the fast path wrote %v", rec.order)
	}
}

// ---- files_changed --------------------------------------------------------

// Counted from the BLOBS. A count-derived value reads 0 for a repository where
// every file changed and none was added, which is exactly the case an operator
// wants to see.
func TestTheJobLineReportsReusedAndChangedCounts(t *testing.T) {
	// The previous commit is this job's OWN output with exactly one blob
	// altered, so the file COUNT is identical between the two and only content
	// differs — the case a count-derived files_changed reads as 0.
	first, firstRec, _ := counted(t)
	first.runJob(context.Background(), aJob())
	if firstRec.failReason() != "" {
		t.Fatalf("the seeding job failed: %s", firstRec.failReason())
	}
	before := map[string]string{}
	var altered string
	for _, f := range firstRec.files {
		before[f.Path] = f.Blob
		if altered == "" && f.Blob != "" {
			altered = f.Path
		}
	}
	if altered == "" {
		t.Fatal("no file has a blob; this fixture cannot detect a changed one")
	}
	before[altered] = "0000000000000000000000000000000000000000"

	ix, rec, _ := counted(t)
	prime(ix, vecOf(ix))
	ix.prevBlobs = func(context.Context, string, string) (map[string]string, error) { return before, nil }
	ix.runJob(context.Background(), aJob())
	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	if len(rec.files) != len(before) {
		t.Fatalf("the two commits have %d and %d files; a count-derived number could see this",
			len(rec.files), len(before))
	}
	log := rec.logged.String()
	if !strings.Contains(log, `"reused":`) {
		t.Errorf("the job line does not report reused: %s", log)
	}
	if !strings.Contains(log, `"files_changed":1`) {
		t.Errorf("the job line does not report files_changed=1: %s", log)
	}
}

func TestChangedFilesCountsBlobsAndNotFileCounts(t *testing.T) {
	now := []models.File{
		{Path: "a.go", Blob: "aaa"},
		{Path: "b.go", Blob: "bbb"},
	}
	before := map[string]string{"a.go": "aaa", "b.go": "OLD"}
	// The file COUNT is identical and one blob changed: a count-derived number
	// reads 0 here.
	if got := changedFiles(now, before); got != 1 {
		t.Errorf("changedFiles = %d, want 1", got)
	}
	if got := changedFiles(now, map[string]string{"a.go": "aaa", "b.go": "bbb"}); got != 0 {
		t.Errorf("changedFiles = %d, want 0 when nothing changed", got)
	}
	// A file that is new to this commit counts as changed.
	if got := changedFiles(now, map[string]string{"a.go": "aaa"}); got != 1 {
		t.Errorf("changedFiles = %d, want 1 for an added file", got)
	}
	// No previous commit is -1, a distinguishable state rather than "nothing
	// changed".
	if got := changedFiles(now, nil); got != -1 {
		t.Errorf("changedFiles with no previous commit = %d, want -1", got)
	}
}

// ---- the reuse read's own shape -------------------------------------------

func TestReuseAsksForEachDigestOnceAndForThisWorkersEmbedder(t *testing.T) {
	ix, _, _ := counted(t, withFiles(map[string]string{
		"one/util.go": "package p\n\nfunc Helper() int { return 1 }\n",
		"two/util.go": "package p\n\nfunc Helper() int { return 1 }\n",
	}))
	var asked []string
	var model string
	var dim int
	ix.embeddings = func(_ context.Context, digests []string, m string, d int) (map[string][]float32, error) {
		asked, model, dim = digests, m, d
		return nil, nil
	}
	ix.runJob(context.Background(), aJob())
	if model != ix.emb.Model() || dim != ix.emb.Dim() {
		t.Errorf("reuse asked for %s/%d, want this worker's %s/%d", model, dim, ix.emb.Model(), ix.emb.Dim())
	}
	seen := map[string]bool{}
	for _, d := range asked {
		if seen[d] {
			t.Errorf("digest %s was asked for twice", d)
		}
		seen[d] = true
		// DIGESTS, not span ids. A span id contains the repo id, which contains
		// the commit, so a span-id-keyed reuse could never hit across commits —
		// which is every case this exists for — while still hitting within one
		// commit's retry, so it would look like it worked.
		if !isDigest(d) {
			t.Errorf("reuse asked for %q, which is not a digest", d)
		}
	}
	if len(asked) == 0 {
		t.Fatal("reuse asked for nothing")
	}
	// The fixture's two files share a digest, so the DISTINCT list is shorter
	// than the span list — which is what "once per digest" means.
	if len(asked) >= 4 {
		t.Errorf("reuse asked for %d digests: %v", len(asked), asked)
	}
}
