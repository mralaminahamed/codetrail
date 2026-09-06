package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/clone"
	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/symbols"
	"github.com/mralaminahamed/codetrail/packages/shared/walk"
)

type failCall struct {
	id, worker, reason string
	max                int
}

// fakeQueue hands out queued jobs one per Lease and records what was done to
// them. stop is called once the jobs run out, so a test can drive run() to
// completion without a timer.
type fakeQueue struct {
	queued      []jobs.Job
	leaseErr    error
	completeErr error
	failErr     error
	stop        func()
	// blockUntilDone makes the two writes wait for their context, which is how
	// a pool that has stopped answering behaves.
	blockUntilDone bool

	leases    []string
	leaseFor  []time.Duration
	completed []string
	// repoIDs is what each Complete named as the job's product; it is the only
	// route from a submission to the repository it produced.
	repoIDs []string
	failed  []failCall
	// The error state of the context each write was made with. A job that ran
	// out of time is the one whose failure most needs recording, and a write on
	// the context that just expired records nothing.
	completedCtx []error
	failedCtx    []error

	// sweepErr fails the retention sweep; sweptFor records the window each call
	// asked for, and its length is the call count.
	sweepErr  error
	sweptFor  []time.Duration
	sweptWhen []time.Time
	// clock, when set, is what Sweep stamps sweptWhen from.
	clock func() time.Time
}

func (q *fakeQueue) Lease(_ context.Context, worker string, d time.Duration) (jobs.Job, bool, error) {
	q.leases = append(q.leases, worker)
	q.leaseFor = append(q.leaseFor, d)
	if q.leaseErr != nil {
		return jobs.Job{}, false, q.leaseErr
	}
	if len(q.queued) == 0 {
		if q.stop != nil {
			q.stop()
		}
		return jobs.Job{}, false, nil
	}
	j := q.queued[0]
	q.queued = q.queued[1:]
	return j, true, nil
}

func (q *fakeQueue) Complete(ctx context.Context, id, worker, repoID string) error {
	if q.blockUntilDone {
		<-ctx.Done()
	}
	q.completed = append(q.completed, id+"@"+worker)
	q.repoIDs = append(q.repoIDs, repoID)
	q.completedCtx = append(q.completedCtx, ctx.Err())
	return q.completeErr
}

func (q *fakeQueue) Fail(ctx context.Context, id, worker, reason string, max int) error {
	if q.blockUntilDone {
		<-ctx.Done()
	}
	q.failed = append(q.failed, failCall{id, worker, reason, max})
	q.failedCtx = append(q.failedCtx, ctx.Err())
	return q.failErr
}

func (q *fakeQueue) Sweep(_ context.Context, keepFor time.Duration) (int, error) {
	q.sweptFor = append(q.sweptFor, keepFor)
	if q.clock != nil {
		q.sweptWhen = append(q.sweptWhen, q.clock())
	}
	if q.sweepErr != nil {
		return 0, q.sweepErr
	}
	return 0, nil
}

const testWorker = "host-0123456789abcdef"

// testIndexer wires an indexer whose every step fails the test if it is
// reached. A test overrides only the steps it means to exercise, so "the clone
// never ran" needs no assertion of its own.
func testIndexer(t *testing.T, q *fakeQueue) (*indexer, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	ix := &indexer{
		// logger.New pins production to InfoLevel; a test logger that accepts
		// more would pass on a line production never writes.
		log:     zerolog.New(&logged).Level(zerolog.InfoLevel),
		q:       q,
		hosts:   admit.NewPolicy(admit.DefaultHosts),
		id:      testWorker,
		scratch: t.TempDir(),
		lim: limits{
			clone: clone.Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second},
			walk:  walk.Limits{MaxFiles: 10, MaxFileBytes: 1 << 10},
			// Not the production default of 3: a literal 3 in place of
			// ix.lim.tries would otherwise be invisible here.
			tries: 4,
			poll:  time.Millisecond,
			// Likewise distinct from every other number here, so a call that
			// passes the wrong one is visible.
			keepRepos:      7,
			keepTombstones: 9,
			jobHistory:     11 * time.Hour,
			sweepEvery:     13 * time.Minute,
			// Small enough that a fixture of a few spans crosses batches: a
			// batch loop that stops after the first is invisible at 32.
			embedBatch: 2,
			// Production's default, so a test that means to exercise the
			// stage does not have to turn it on; the resolver below is what
			// keeps it from running unasked. goBin is a value nothing on this
			// machine resolves to, so a Policy built from anything else is
			// visible.
			typecheck: true,
			goBin:     "/opt/codetrail-test/go",
			goProxy:   symbols.ProxyOff,
			// Off by default here and ON in production, deliberately: every
			// test written before P7 drives the cloning path, and a fast path
			// that silently short-circuited them would make those tests assert
			// nothing. The fast-path tests turn it on explicitly.
			skipClone: false,
			reuse:     true,
		},
		// The embedder is a dependency, not a step: anything that reaches the
		// chunker needs one, and spec §9 puts the fake in CI.
		emb: embed.NewFake(store.EmbeddingDim),
		opt: chunk.Defaults(),
		clone: func(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
			t.Error("clone ran when it should not have")
			return clone.Result{}, errors.New("unexpected clone")
		},
		walk: func(context.Context, string, walk.Limits) ([]walk.File, error) {
			t.Error("walk ran when it should not have")
			return nil, errors.New("unexpected walk")
		},
		put: func(context.Context, models.Repo, []models.File) error {
			t.Error("put ran when it should not have")
			return errors.New("unexpected put")
		},
		putSpans: func(context.Context, string, []store.EmbeddedSpan, string, int) error {
			t.Error("putSpans ran when it should not have")
			return errors.New("unexpected putSpans")
		},
		graph: func(context.Context, symbols.Policy) (map[symbols.Key]symbols.Target, symbols.Stats) {
			t.Error("the type-checker ran when it should not have")
			return nil, symbols.Stats{Reason: symbols.ReasonLoadError}
		},
		putGraph: func(context.Context, string, []models.Symbol, []models.Edge) error {
			t.Error("putGraph ran when it should not have")
			return errors.New("unexpected putGraph")
		},
		evict: func(context.Context, int, int) (int, error) {
			t.Error("evict ran when it should not have")
			return 0, errors.New("unexpected evict")
		},
		// A cleanup on the terminal-failure path rather than a step a test
		// opts into, so it answers instead of failing the test: every job that
		// fails against its attempt budget reaches it, and a stub that failed
		// there would make "this job was retried" untestable.
		dropRepo: func(context.Context, string) (bool, error) { return false, nil },

		// The incremental re-index's five reads. skipClone is OFF here so every
		// existing test still drives the cloning path unchanged; a test that
		// means to exercise the fast path turns it on and replaces resolve.
		//
		// resolve fails the test if it runs, like every other step. The four
		// store reads answer rather than failing, because they are reads a job
		// makes on its way past rather than steps a test opts into: reuse is an
		// optimisation whose absence must not change any other assertion.
		resolve: func(context.Context, string, string, clone.Limits) (string, error) {
			t.Error("resolve ran when it should not have")
			return "", errors.New("unexpected resolve")
		},
		getRepo: func(context.Context, string) (models.Repo, error) {
			return models.Repo{}, store.ErrNotFound
		},
		countSpans:   func(context.Context, string) (int, error) { return 0, nil },
		spanEmbedder: func(context.Context, string) (string, int, error) { return "", 0, store.ErrNotFound },
		embeddings: func(context.Context, []string, string, int) (map[string][]float32, error) {
			return map[string][]float32{}, nil
		},
		prevBlobs: func(context.Context, string, string) (map[string]string, error) {
			return map[string]string{}, nil
		},
		now: time.Now,
	}
	return ix, &logged
}

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func okPutSpans(context.Context, string, []store.EmbeddedSpan, string, int) error { return nil }

func okPutGraph(context.Context, string, []models.Symbol, []models.Edge) error { return nil }

// okGraph is a type-checker that names nothing: every edge stays syntactic,
// which is what a job that only means to reach Complete needs from it.
func okGraph(context.Context, symbols.Policy) (map[symbols.Key]symbols.Target, symbols.Stats) {
	return nil, symbols.Stats{Reason: symbols.ReasonOK}
}

// writeCheckout puts the given files on disk under dir. The chunk pass reads
// every walked file a second time, so a walk that names a file the checkout
// does not hold is a job that fails on the missing file.
func writeCheckout(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Two files with different shapes: the Go one takes the AST arm, the markdown
// one the window fallback, so a job that indexed only one of them is visible.
const (
	mainGo   = "package main\n\nfunc main() {}\n"
	readmeMD = "# hi\n"
)

func aJob() jobs.Job {
	return jobs.Job{ID: "job1", Remote: "https://github.com/a/b", Ref: "main", Status: jobs.StatusLeased, Attempts: 1}
}

// The caps must be positive or clone.Limits and walk.Limits refuse per job:
// a knob set to "0" would fail every repository in the queue to terminal
// rather than fail one boot, and config.GetInt reads "0" as 0, not as unset.
func TestLimitsRefuseANonPositiveKnob(t *testing.T) {
	for _, key := range []string{
		"MAX_REPO_BYTES", "JOB_DEADLINE_SECONDS", "MAX_REPO_FILES",
		"MAX_FILE_BYTES", "MAX_ATTEMPTS", "POLL_SECONDS", "KEEP_REPOS", "EMBED_BATCH",
		"KEEP_TOMBSTONES", "JOB_HISTORY_HOURS", "JOB_SWEEP_MINUTES",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "0")
			if _, err := limitsFrom(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("want an error naming %s, got %v", key, err)
			}
			t.Setenv(key, "-1")
			if _, err := limitsFrom(); err == nil {
				t.Fatalf("%s=-1 was accepted", key)
			}
		})
	}
	if _, err := limitsFrom(); err != nil {
		t.Fatalf("the defaults must boot: %v", err)
	}
}

// Every integer knob the indexer reads, with a letter O for a zero. Before
// this, all fifteen booted on their defaults and logged "indexer up": a worker
// keeping 50 repositories while its operator wrote 5O is exactly the silence
// the positive check above exists to prevent, one step earlier.
func TestEveryIntegerKnobRefusesAValueThatIsNotAnInteger(t *testing.T) {
	for _, key := range []string{
		"MAX_REPO_BYTES", "JOB_DEADLINE_SECONDS", "MAX_REPO_FILES",
		"MAX_FILE_BYTES", "MAX_ATTEMPTS", "POLL_SECONDS", "KEEP_REPOS", "EMBED_BATCH",
		"KEEP_TOMBSTONES", "JOB_HISTORY_HOURS", "JOB_SWEEP_MINUTES",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "4O")
			_, err := limitsFrom()
			refusalNames(t, err, key)
		})
	}
	for _, key := range []string{"CHUNK_WINDOW_LINES", "CHUNK_WINDOW_OVERLAP", "CHUNK_MAX_DECL_LINES"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "4O")
			_, err := chunkOptions()
			refusalNames(t, err, key)
		})
	}
	t.Run("EMBED_DIM", func(t *testing.T) {
		t.Setenv("EMBED_PROVIDER", "fake")
		t.Setenv("EMBED_DIM", "4O")
		_, err := newEmbedder(context.Background(), time.Minute)
		refusalNames(t, err, "EMBED_DIM")
	})
}

// A refusal an operator can act on names the setting and the value they typed.
func refusalNames(t *testing.T, err error, key string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s=4O booted", key)
	}
	if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "4O") {
		t.Errorf("the error names neither the setting nor the value: %v", err)
	}
}

// Every knob has to reach the cap it names. A crossed pair is silent: the
// worker boots, clones and indexes, with the wrong two numbers in force.
func TestLimitsAreWiredToTheCapsTheyName(t *testing.T) {
	t.Setenv("MAX_REPO_BYTES", "11")
	t.Setenv("JOB_DEADLINE_SECONDS", "22")
	t.Setenv("MAX_REPO_FILES", "33")
	t.Setenv("MAX_FILE_BYTES", "44")
	t.Setenv("MAX_ATTEMPTS", "55")
	t.Setenv("POLL_SECONDS", "66")
	t.Setenv("KEEP_REPOS", "77")
	t.Setenv("EMBED_BATCH", "88")
	t.Setenv("KEEP_TOMBSTONES", "99")
	t.Setenv("JOB_HISTORY_HOURS", "111")
	t.Setenv("JOB_SWEEP_MINUTES", "222")
	t.Setenv("TYPECHECK", "false")
	t.Setenv("TYPECHECK_GOPROXY", "https://proxy.example")
	// REINDEX_SKIP_CLONE and REINDEX_REUSE are deliberately left unset: their
	// defaults are what this test pins. Their parsing is pinned separately by
	// TestReuseKnobsAreParsedNotCompared.
	lim, err := limitsFrom()
	if err != nil {
		t.Fatal(err)
	}
	want := limits{
		clone:          clone.Limits{MaxBytes: 11, Deadline: 22 * time.Second},
		walk:           walk.Limits{MaxFiles: 33, MaxFileBytes: 44},
		tries:          55,
		poll:           66 * time.Second,
		keepRepos:      77,
		keepTombstones: 99,
		embedBatch:     88,
		jobHistory:     111 * time.Hour,
		sweepEvery:     222 * time.Minute,
		typecheck:      false,
		// Not a knob: whatever this machine's PATH resolves, which is the
		// binary go/packages would fork. Empty here would mean no toolchain,
		// which is a downgrade rather than a boot failure.
		goBin:   goToolchain(),
		goProxy: "https://proxy.example",
		// Both DEFAULT TO TRUE, and this test does not set either environment
		// variable — so the values here are the shipped defaults, asserted in
		// the one place every other cap's default is.
		skipClone: true,
		reuse:     true,
	}
	if lim != want {
		t.Fatalf("want %+v, got %+v", want, lim)
	}
}

// jobs.Complete and jobs.Fail authorise on the worker id, so two indexers
// sharing one would silently finish each other's jobs and raise no error
// anywhere. Two ids from one binary must differ.
func TestWorkerIDIsUniquePerProcess(t *testing.T) {
	a, b := workerID(), workerID()
	if a == b {
		t.Fatalf("two workers share the id %q: each can complete the other's jobs", a)
	}
	host, err := os.Hostname()
	if err == nil && host != "" && !strings.HasPrefix(a, host) {
		t.Errorf("want %q to start with the hostname %q, so leased_by says where the worker is", a, host)
	}
}

func TestRunJobIndexesAndCompletes(t *testing.T) {
	q := &fakeQueue{}
	ix, _ := testIndexer(t, q)
	job := aJob()
	// Under the worker's own subtree, not the shared root: two indexers on one
	// SCRATCH_DIR otherwise collide on scratch/<job>, and each one's RemoveAll
	// destroys the other's checkout.
	dir := filepath.Join(ix.scratch, ix.id, job.ID)

	ix.clone = func(_ context.Context, remote, ref, d string, lim clone.Limits) (clone.Result, error) {
		if remote != job.Remote || ref != job.Ref {
			t.Errorf("clone got %q@%q, want %q@%q", remote, ref, job.Remote, job.Ref)
		}
		if d != dir {
			t.Errorf("clone got dir %q, want %q", d, dir)
		}
		if lim != ix.lim.clone {
			t.Errorf("clone got limits %+v, want %+v", lim, ix.lim.clone)
		}
		writeCheckout(t, d, map[string]string{"main.go": mainGo, "a/b.md": readmeMD})
		return clone.Result{Dir: d, Commit: testCommit, Bytes: 4096}, nil
	}
	ix.walk = func(_ context.Context, root string, lim walk.Limits) ([]walk.File, error) {
		if root != dir {
			t.Errorf("walk got root %q, want the clone's dir %q", root, dir)
		}
		if lim != ix.lim.walk {
			t.Errorf("walk got limits %+v, want %+v", lim, ix.lim.walk)
		}
		return []walk.File{
			{Path: "main.go", Lang: "go", Lines: 3, Bytes: 40},
			{Path: "a/b.md", Lang: "markdown", Lines: 1, Bytes: 9},
		}, nil
	}
	var gotRepo models.Repo
	var gotFiles []models.File
	ix.put = func(_ context.Context, r models.Repo, f []models.File) error {
		gotRepo, gotFiles = r, f
		return nil
	}
	var gotSpans []store.EmbeddedSpan
	var gotModel string
	var gotDim int
	ix.putSpans = func(_ context.Context, r string, sp []store.EmbeddedSpan, model string, dim int) error {
		if r != store.RepoID("github.com/a/b", testCommit) {
			t.Errorf("putSpans got repo %q", r)
		}
		gotSpans, gotModel, gotDim = sp, model, dim
		return nil
	}
	ix.graph, ix.putGraph = okGraph, okPutGraph
	var keeps []int
	ix.evict = func(_ context.Context, keep, keepTombstones int) (int, error) {
		keeps = append(keeps, keep)
		if keepTombstones != ix.lim.keepTombstones {
			t.Errorf("evicted keeping %d tombstones, want %d", keepTombstones, ix.lim.keepTombstones)
		}
		return 0, nil
	}

	ix.runJob(context.Background(), job)

	// Keyed on admit's identity, not on the row's spelling.
	repoID := store.RepoID("github.com/a/b", testCommit)
	wantRepo := models.Repo{ID: repoID, Remote: job.Remote, Ref: job.Ref, Commit: testCommit, SizeBytes: 4096}
	if gotRepo != wantRepo {
		t.Errorf("repo: want %+v, got %+v", wantRepo, gotRepo)
	}
	want := []models.File{
		{ID: store.FileID(repoID, "main.go"), RepoID: repoID, Path: "main.go",
			Blob: store.BlobHash([]byte(mainGo)), Lang: "go", Lines: 3},
		{ID: store.FileID(repoID, "a/b.md"), RepoID: repoID, Path: "a/b.md",
			Blob: store.BlobHash([]byte(readmeMD)), Lang: "markdown", Lines: 1},
	}
	if len(gotFiles) != len(want) {
		t.Fatalf("want %d files, got %d", len(want), len(gotFiles))
	}
	for i := range want {
		if gotFiles[i] != want[i] {
			t.Errorf("file %d: want %+v, got %+v", i, want[i], gotFiles[i])
		}
	}
	// The AST arm for the Go file, the window fallback for the markdown one —
	// with the text, the range and the digest each row is cited by.
	wantSpans := []models.Span{
		{ID: store.SpanID(repoID, "main.go", 3, 3, store.Digest("func main() {}")),
			RepoID: repoID, FileID: store.FileID(repoID, "main.go"), Path: "main.go",
			Kind: models.KindFunc, Symbol: "main", StartLine: 3, EndLine: 3,
			Text: "func main() {}", Digest: store.Digest("func main() {}")},
		{ID: store.SpanID(repoID, "a/b.md", 1, 1, store.Digest("# hi")),
			RepoID: repoID, FileID: store.FileID(repoID, "a/b.md"), Path: "a/b.md",
			Kind: models.KindFile, StartLine: 1, EndLine: 1,
			Text: "# hi", Digest: store.Digest("# hi")},
	}
	if len(gotSpans) != len(wantSpans) {
		t.Fatalf("want %d spans, got %d: %+v", len(wantSpans), len(gotSpans), gotSpans)
	}
	for i := range wantSpans {
		if gotSpans[i].Span != wantSpans[i] {
			t.Errorf("span %d: want %+v, got %+v", i, wantSpans[i], gotSpans[i].Span)
		}
		if len(gotSpans[i].Embedding) != store.EmbeddingDim {
			t.Errorf("span %d carries %d components, want %d", i, len(gotSpans[i].Embedding), store.EmbeddingDim)
		}
	}
	// Written per row, so a corpus built with the fake is identifiable in the
	// data rather than from whoever remembers how it was run.
	if gotModel != ix.emb.Model() || gotDim != ix.emb.Dim() {
		t.Errorf("spans recorded model %q dim %d, want %q and %d", gotModel, gotDim, ix.emb.Model(), ix.emb.Dim())
	}
	if len(q.completed) != 1 || q.completed[0] != job.ID+"@"+testWorker {
		t.Errorf("want the job completed as this worker, got %v", q.completed)
	}
	if len(q.failed) != 0 {
		t.Errorf("nothing failed, yet: %v", q.failed)
	}
	// The corpus grew by one repo, so the quota is checked with the configured
	// keep — not a literal, which would ignore KEEP_REPOS entirely.
	if len(keeps) != 1 || keeps[0] != ix.lim.keepRepos {
		t.Errorf("want one eviction keeping %d, got %v", ix.lim.keepRepos, keeps)
	}
}

// The scratch tree goes on every path. A failed job that leaves its checkout
// behind fills the disk one failure at a time, and clone.Run's own cleanup
// explicitly does not cover a descendant that outlived the process-group kill.
func TestRunJobRemovesTheScratchTreeOnEveryPath(t *testing.T) {
	okClone := func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o700); err != nil {
			return clone.Result{}, err
		}
		return clone.Result{Dir: d, Commit: testCommit}, os.WriteFile(filepath.Join(d, "sub", "f.go"), []byte("package p\n"), 0o600)
	}
	okWalk := func(context.Context, string, walk.Limits) ([]walk.File, error) {
		return []walk.File{{Path: "sub/f.go", Lang: "go", Lines: 1}}, nil
	}

	for _, tc := range []struct {
		name  string
		setup func(ix *indexer)
		fails bool
	}{
		{"clone fails", func(ix *indexer) {
			ix.clone = func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
				_ = os.MkdirAll(d, 0o700)
				return clone.Result{}, errors.New("clone exploded")
			}
		}, true},
		{"walk fails", func(ix *indexer) {
			ix.clone = okClone
			ix.walk = func(context.Context, string, walk.Limits) ([]walk.File, error) {
				return nil, errors.New("walk exploded")
			}
		}, true},
		{"write fails", func(ix *indexer) {
			ix.clone, ix.walk = okClone, okWalk
			ix.put = func(context.Context, models.Repo, []models.File) error { return errors.New("write exploded") }
		}, true},
		{"the span write fails", func(ix *indexer) {
			ix.clone, ix.walk = okClone, okWalk
			ix.put = func(context.Context, models.Repo, []models.File) error { return nil }
			ix.putSpans = func(context.Context, string, []store.EmbeddedSpan, string, int) error {
				return errors.New("spans exploded")
			}
		}, true},
		{"everything works", func(ix *indexer) {
			ix.clone, ix.walk = okClone, okWalk
			ix.put = func(context.Context, models.Repo, []models.File) error { return nil }
			ix.putSpans = okPutSpans
			ix.graph, ix.putGraph = okGraph, okPutGraph
			ix.evict = func(context.Context, int, int) (int, error) { return 0, nil }
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQueue{}
			ix, _ := testIndexer(t, q)
			tc.setup(ix)
			job := aJob()
			ix.runJob(context.Background(), job)

			if _, err := os.Stat(filepath.Join(ix.scratch, ix.id, job.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the checkout is still on disk: %v", err)
			}
			if tc.fails {
				if len(q.failed) != 1 || q.failed[0].max != ix.lim.tries || q.failed[0].worker != testWorker {
					t.Errorf("want one failure against the attempt budget, got %+v", q.failed)
				}
			} else if len(q.completed) != 1 {
				t.Errorf("want the job completed, got %v", q.completed)
			}
		})
	}
}

// git clone refuses a non-empty destination, so a checkout left by a crash
// would fail every retry of that job on the leftover rather than on the
// repository.
func TestRunJobClearsALeftoverCheckoutBeforeCloning(t *testing.T) {
	q := &fakeQueue{}
	ix, _ := testIndexer(t, q)
	job := aJob()
	dir := filepath.Join(ix.scratch, ix.id, job.ID)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var sawLeftover bool
	ix.clone = func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
		_, err := os.Stat(d)
		sawLeftover = err == nil
		return clone.Result{}, errors.New("stop here")
	}
	ix.runJob(context.Background(), job)

	if sawLeftover {
		t.Error("the crashed run's checkout was still there when git was called: the retry fails on the leftover")
	}
}

// A full commit sha cannot be cloned: git clone --branch takes a ref name and
// answers "Remote branch <sha> not found in upstream origin" (measured). The
// API's ref check accepts one, so the refusal has to be here — and it is
// permanent, because no retry and no configuration change can make it clone.
func TestACommitSHARefIsRefusedPermanentlyWithoutCloning(t *testing.T) {
	for _, ref := range []string{
		testCommit,
		strings.ToUpper(testCommit),
		strings.Repeat("ab", 32), // sha256 object format
	} {
		q := &fakeQueue{}
		ix, _ := testIndexer(t, q)
		job := aJob()
		job.Ref = ref
		ix.runJob(context.Background(), job)

		if len(q.failed) != 1 || q.failed[0].max != 0 {
			t.Fatalf("%s: want one permanent failure, got %+v", ref, q.failed)
		}
		if !strings.Contains(q.failed[0].reason, "sha") {
			t.Errorf("%s: the reason does not say why: %q", ref, q.failed[0].reason)
		}
	}
	// A branch name is not a sha, whatever it looks like.
	for _, ref := range []string{"main", "deadbeef", "v1.2.3", testCommit[:39], testCommit[:39] + "g"} {
		q := &fakeQueue{}
		ix, _ := testIndexer(t, q)
		ix.clone = func(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
			return clone.Result{}, errors.New("stop here")
		}
		job := aJob()
		job.Ref = ref
		ix.runJob(context.Background(), job)
		if len(q.failed) != 1 || q.failed[0].max != ix.lim.tries {
			t.Errorf("%s: a real ref must be attempted against the budget, got %+v", ref, q.failed)
		}
	}
}

// jobs.Fail's attempt cap only binds when someone calls Fail. A job that kills
// its indexer never does: the lease expires, the next worker leases it, and
// attempts climbs while status stays 'leased' — forever. The worker holding
// the lease is the one that can end it.
func TestAJobBeyondItsAttemptBudgetIsFailedPermanently(t *testing.T) {
	q := &fakeQueue{}
	ix, _ := testIndexer(t, q)
	job := aJob()
	job.Attempts = ix.lim.tries + 1
	ix.runJob(context.Background(), job)

	if len(q.failed) != 1 || q.failed[0].max != 0 {
		t.Fatalf("want one permanent failure, got %+v", q.failed)
	}

	// The boundary: the last attempt within the budget still runs. Refusing it
	// would lose the retry the budget exists to give.
	q = &fakeQueue{}
	ix, _ = testIndexer(t, q)
	ix.clone = func(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
		return clone.Result{}, errors.New("stop here")
	}
	job = aJob()
	job.Attempts = ix.lim.tries
	ix.runJob(context.Background(), job)
	if len(q.failed) != 1 || q.failed[0].max != ix.lim.tries {
		t.Fatalf("the final budgeted attempt must run, got %+v", q.failed)
	}
}

// Losing the lease is normal — the job was reclaimed while this worker ran.
// Silently dropping the error is not: it is the only sign that this worker's
// verdict was never recorded against the job it thought it held.
//
// And only ErrNotLeased means the lease is gone. Under SIGTERM mid-clone the
// error is a cancelled context and the lease is still held; a line saying
// otherwise tells an operator something the code has not established.
func TestALostLeaseIsLogged(t *testing.T) {
	lost := "lease no longer held"
	for _, tc := range []struct {
		name     string
		err      error
		wantLost bool
	}{
		{"complete loses the lease", jobs.ErrNotLeased, true},
		{"complete is cancelled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQueue{completeErr: tc.err}
			ix, logged := testIndexer(t, q)
			ix.clone = func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
				return clone.Result{Dir: d, Commit: testCommit}, nil
			}
			ix.walk = func(context.Context, string, walk.Limits) ([]walk.File, error) { return nil, nil }
			ix.put = func(context.Context, models.Repo, []models.File) error { return nil }
			ix.putSpans = okPutSpans
			ix.graph, ix.putGraph = okGraph, okPutGraph

			ix.runJob(context.Background(), aJob())
			out := logged.String()
			if !strings.Contains(out, "could not complete") {
				t.Errorf("a failed completion left no trace: %q", out)
			}
			if got := strings.Contains(out, lost); got != tc.wantLost {
				t.Errorf("said %q: %v, want %v — %q", lost, got, tc.wantLost, out)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		err      error
		wantLost bool
	}{
		{"fail loses the lease", jobs.ErrNotLeased, true},
		{"fail is cancelled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQueue{failErr: tc.err}
			ix, logged := testIndexer(t, q)
			ix.clone = func(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
				return clone.Result{}, errors.New("clone exploded")
			}
			ix.runJob(context.Background(), aJob())
			out := logged.String()
			if !strings.Contains(out, "could not record failure") {
				t.Errorf("a failure that was never recorded left no trace: %q", out)
			}
			if got := strings.Contains(out, lost); got != tc.wantLost {
				t.Errorf("said %q: %v, want %v — %q", lost, got, tc.wantLost, out)
			}
		})
	}
}

// The scratch tree is this worker's alone, and a sweep must not reach into a
// peer's: nothing here can tell a crashed worker's directory from a live one's.
func TestSweepHomeLeavesAPeersTreeAlone(t *testing.T) {
	q := &fakeQueue{}
	ix, _ := testIndexer(t, q)
	peer := filepath.Join(ix.scratch, "another-worker", "job9")
	if err := os.MkdirAll(peer, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(ix.home(), "job1")
	if err := os.MkdirAll(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ix.home(), filepath.Join(ix.scratch, ix.id)) {
		t.Fatalf("home %q is not this worker's own subtree", ix.home())
	}

	ix.sweepHome()

	if _, err := os.Stat(mine); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("this worker's tree survived its own sweep: %v", err)
	}
	if _, err := os.Stat(peer); err != nil {
		t.Errorf("a peer's checkout was swept away: %v", err)
	}
}

// A repository over the size cap is refused for good, and everything else the
// clone can fail with keeps its attempts.
//
// The cap is checked after the fetch — nothing bounds what reaches the disk
// while git runs except the deadline (clone.Run) — so a retry is a second and
// third full download of a repository that cannot fit, up to 3 × MAX_REPO_BYTES
// through the ephemeral volume for one anonymous submission.
//
// The transport failure is asserted alongside, because "terminal" is only a
// property if something is still retried: a mutant that failed every clone
// finally would pass the first case on its own.
func TestAnOverSizedRepositoryIsRefusedForGoodAndOtherCloneFailuresAreRetried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantMax int // 0 is terminal; the attempt budget is a retry
	}{
		{"over the size cap", fmt.Errorf("%w: 300 bytes > 200", clone.ErrTooLarge), 0},
		{"the forge hung up", errors.New("clone: exit status 128: could not read from remote"), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQueue{}
			ix, _ := testIndexer(t, q)
			ix.clone = func(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
				return clone.Result{}, tc.err
			}
			want := tc.wantMax
			if want == -1 {
				want = ix.lim.tries
			}
			ix.runJob(context.Background(), aJob())

			if len(q.failed) != 1 || q.failed[0].max != want {
				t.Fatalf("want one failure with max %d, got %+v", want, q.failed)
			}
			if !strings.Contains(q.failed[0].reason, tc.err.Error()) {
				t.Errorf("the recorded reason is %q, want git's own words", q.failed[0].reason)
			}
		})
	}
}

// A job that dies terminally between PutRepo and PutSpans must not leave the
// repository behind.
//
// The three writes are three transactions, so the repo row and its files are
// committed while the spans are not. GET /repos then lists a repository /ask
// refuses no_spans forever — and because PutRepo winds last_queried_at it is
// the MOST recently "queried" row, so Evict takes it last and it holds a quota
// slot indefinitely. Nothing else sweeps a repo with zero spans.
//
// The retry case is asserted alongside, and it is the half that says why this
// is not simply part of fail(): an attempt that will be tried again writes the
// same repo id, and a worker whose lease expired mid-job must not delete the
// row the worker that took the job is filling in.
func TestATerminalFailureAfterTheRepoWriteRemovesTheRepo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int
		wantDrop bool
	}{
		{"the last attempt", 4, true},
		{"an attempt that will be retried", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ix, rec := fakeIndexer(t)
			ix.putSpans = func(context.Context, string, []store.EmbeddedSpan, string, int) error {
				return errors.New("spans exploded")
			}
			job := aJob()
			job.Attempts = tc.attempts
			ix.runJob(context.Background(), job)

			if len(rec.failed) != 1 {
				t.Fatalf("want one failure, got %+v", rec.failed)
			}
			want := []string{store.RepoID("github.com/a/b", testCommit)}
			if !tc.wantDrop {
				want = nil
			}
			if !slices.Equal(rec.dropped, want) {
				t.Errorf("the cleanup was asked to remove %v, want %v", rec.dropped, want)
			}
		})
	}
}

// The cleanup is a repair, not an outcome: it must not turn a recorded failure
// into a different one, and a database that will not answer it must not cost
// the worker anything more than a log line.
func TestAFailedCleanupIsLoggedAndChangesNothingElse(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.putSpans = func(context.Context, string, []store.EmbeddedSpan, string, int) error {
		return errors.New("spans exploded")
	}
	ix.dropRepo = func(context.Context, string) (bool, error) {
		return false, errors.New("postgres went away")
	}
	job := aJob()
	job.Attempts = ix.lim.tries
	ix.runJob(context.Background(), job)

	if len(rec.failed) != 1 || rec.failed[0].reason != "spans exploded" {
		t.Fatalf("the recorded failure is %+v, want the span write's own reason", rec.failed)
	}
	if out := rec.logged.String(); !strings.Contains(out, "postgres went away") {
		t.Errorf("the cleanup failure is not in the log: %s", out)
	}
}

// ALLOWED_HOSTS is the only SSRF control codetrail has (admit's package doc),
// and clearing it must grant NOTHING rather than hand the default back.
//
// Read through config.Get it failed open, because Get treats present-but-empty
// as unset. Measured:
//
//	ALLOWED_HOSTS=""   -> ["github.com" "codeberg.org"], github.com ADMITTED
//	ALLOWED_HOSTS=" "  -> [" "],                         github.com refused
//
// A single space was the difference between fail-open and fail-closed, for an
// operator clearing the control on purpose.
//
// Asserted through the POLICY and not through the slice: an empty list that
// admit still admitted from would pass a length check and refuse nothing.
func TestClearingTheAllowlistAdmitsNoHost(t *testing.T) {
	for _, v := range []string{"", " "} {
		t.Run(strconv.Quote(v), func(t *testing.T) {
			t.Setenv("ALLOWED_HOSTS", v)
			p := admit.NewPolicy(allowedHosts())
			for _, remote := range []string{"https://github.com/a/b", "https://codeberg.org/a/b"} {
				if _, err := p.Check(remote); err == nil {
					t.Errorf("ALLOWED_HOSTS=%q admitted %s: the default allowlist is back", v, remote)
				}
			}
		})
	}
	// The control: an unset knob still gets the default, or the assertion above
	// would pass against an allowlist reader that returned nothing ever.
	t.Setenv("ALLOWED_HOSTS", "")
	os.Unsetenv("ALLOWED_HOSTS")
	if _, err := admit.NewPolicy(allowedHosts()).Check("https://github.com/a/b"); err != nil {
		t.Errorf("with ALLOWED_HOSTS unset, github.com is refused: %v", err)
	}
}

// The tree leaked without bound across hard kills: workerID generates a fresh
// id every boot, so sweepHome removes only the running process's own subtree
// and no future worker ever reached a killed one's. Every OOM-kill stranded up
// to MAX_REPO_BYTES for good.
//
// The boot sweep takes what cannot belong to a live worker and nothing else.
// The window is the job's deadline plus the lease that outlasts it, and the
// peer just inside it is the assertion that matters: it is the in-flight clone
// sweepHome's warning is about, and a sweep that reached it would be the
// worker-on-worker collision with extra steps.
func TestTheBootSweepTakesAnAbandonedTreeAndLeavesALiveOne(t *testing.T) {
	q := &fakeQueue{}
	ix, logged := testIndexer(t, q)
	window := 2*ix.lim.clone.Deadline + time.Minute

	trees := map[string]time.Duration{
		"killed-worker":  window + time.Second,
		"working-worker": window - time.Second,
		"fresh-worker":   0,
	}
	for name, age := range trees {
		checkout := filepath.Join(ix.scratch, name, "job1")
		if err := os.MkdirAll(checkout, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkout, "f.go"), []byte("package p\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(ix.scratch, name), at, at); err != nil {
			t.Fatal(err)
		}
	}

	ix.sweepStale()

	for name := range trees {
		_, err := os.Stat(filepath.Join(ix.scratch, name, "job1", "f.go"))
		gone := errors.Is(err, os.ErrNotExist)
		if want := name == "killed-worker"; gone != want {
			t.Errorf("%s: swept=%v, want %v (%v)", name, gone, want, err)
		}
	}
	if out := logged.String(); !strings.Contains(out, "killed-worker") {
		t.Errorf("the sweep says nothing about what it removed: %s", out)
	}
}

// A scratch directory that does not exist yet is the normal first boot, not a
// fault to warn about.
func TestTheBootSweepIsQuietWhenThereIsNothingToSweep(t *testing.T) {
	q := &fakeQueue{}
	ix, logged := testIndexer(t, q)
	ix.scratch = filepath.Join(ix.scratch, "not-created-yet")

	ix.sweepStale()

	if out := logged.String(); out != "" {
		t.Errorf("a first boot logged %s", out)
	}
}

// The binary's doc comment claims this is the component that handles the
// untrusted URL. A row it did not admit — an allowlist edited since, or a
// write that did not come from the gateway — reaches git otherwise.
func TestAnInadmissibleRemoteIsRefusedWithoutCloning(t *testing.T) {
	for _, tc := range []struct {
		remote  string
		wantMax int // 0 is permanent; tries keeps the budget
	}{
		{"file:///etc/passwd", 0},
		{"http://github.com/a/b", 0},
		{"https://github.com/a/b/../../etc", 0},
		{"https://evil.test/a/b", -1}, // -1 means "the attempt budget"
	} {
		q := &fakeQueue{}
		ix, _ := testIndexer(t, q)
		want := tc.wantMax
		if want == -1 {
			want = ix.lim.tries
		}
		job := aJob()
		job.Remote = tc.remote
		ix.runJob(context.Background(), job)

		if len(q.failed) != 1 || q.failed[0].max != want {
			t.Errorf("%s: want one failure with max %d, got %+v", tc.remote, want, q.failed)
		}
	}
}

// run leases as this worker, for longer than the clone may take, and stops
// when the process is asked to.
func TestRunLeasesAsThisWorkerAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &fakeQueue{queued: []jobs.Job{aJob()}, stop: cancel}
	ix, _ := testIndexer(t, q)
	ix.clone = func(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
		return clone.Result{}, errors.New("stop here")
	}

	done := make(chan struct{})
	go func() { defer close(done); ix.run(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after its context was cancelled")
	}

	if len(q.leases) == 0 || q.leases[0] != testWorker {
		t.Fatalf("want the lease taken as %q, got %v", testWorker, q.leases)
	}
	// A lease shorter than the clone's own deadline hands the job to a second
	// worker while the first is still cloning it.
	if q.leaseFor[0] <= ix.lim.clone.Deadline {
		t.Errorf("lease of %s does not outlast a clone allowed %s", q.leaseFor[0], ix.lim.clone.Deadline)
	}
	if len(q.failed) != 1 {
		t.Errorf("want the leased job attempted, got %+v", q.failed)
	}
}

// A cancelled process must not lease one more job on its way out, and must
// return rather than spin: a loop that never reads ctx hangs here instead of
// exiting, which the deadline is what catches.
func TestRunLeasesNothingOnceCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := &fakeQueue{queued: []jobs.Job{aJob()}}
	ix, _ := testIndexer(t, q)

	done := make(chan struct{})
	go func() { defer close(done); ix.run(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run kept going after its context was cancelled")
	}
	if len(q.leases) != 0 {
		t.Fatalf("leased %d jobs after cancellation", len(q.leases))
	}
}

// Eviction runs on a successful index and nowhere else. A successful index is
// the only thing that grows the corpus, so it is the only moment the quota can
// be exceeded — and a job that failed, or one whose completion did not land,
// has no new repo of its own to account for.
func TestEvictionRunsOnlyAfterAnIndexThatCompleted(t *testing.T) {
	okClone := func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
		return clone.Result{Dir: d, Commit: testCommit}, nil
	}
	okWalk := func(context.Context, string, walk.Limits) ([]walk.File, error) { return nil, nil }
	okPut := func(context.Context, models.Repo, []models.File) error { return nil }

	for _, tc := range []struct {
		name string
		q    *fakeQueue
		set  func(ix *indexer)
		want bool
	}{
		{"indexed and completed", &fakeQueue{}, func(ix *indexer) {
			ix.clone, ix.walk, ix.put, ix.putSpans = okClone, okWalk, okPut, okPutSpans
			ix.graph, ix.putGraph = okGraph, okPutGraph
		}, true},
		{"the write failed", &fakeQueue{}, func(ix *indexer) {
			ix.clone, ix.walk = okClone, okWalk
			ix.put = func(context.Context, models.Repo, []models.File) error { return errors.New("write exploded") }
		}, false},
		{"the clone failed", &fakeQueue{}, func(ix *indexer) {
			ix.clone = func(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
				return clone.Result{}, errors.New("clone exploded")
			}
		}, false},
		// The rows are in by now, so this one is a judgement call rather than a
		// necessity: the lease was lost, another worker is finishing the same
		// job, and its own completion evicts. The bound is enforced at the next
		// successful index either way.
		{"the completion was refused", &fakeQueue{completeErr: jobs.ErrNotLeased}, func(ix *indexer) {
			ix.clone, ix.walk, ix.put, ix.putSpans = okClone, okWalk, okPut, okPutSpans
			ix.graph, ix.putGraph = okGraph, okPutGraph
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ix, _ := testIndexer(t, tc.q)
			tc.set(ix)
			var ran bool
			ix.evict = func(_ context.Context, keep, _ int) (int, error) {
				ran = true
				if keep != ix.lim.keepRepos {
					t.Errorf("evicted keeping %d, want %d", keep, ix.lim.keepRepos)
				}
				return 0, nil
			}
			ix.runJob(context.Background(), aJob())
			if ran != tc.want {
				t.Errorf("evict ran: %v, want %v", ran, tc.want)
			}
		})
	}
}

// The job is done and its lease released by the time eviction runs, so there
// is nothing left to retry or fail. An eviction that cannot run must therefore
// say so and leave the job completed — a corpus over quota is a disk problem,
// a job re-run for it would be a correctness one.
func TestAFailedEvictionIsLoggedAndDoesNotUncompleteTheJob(t *testing.T) {
	q := &fakeQueue{}
	ix, logged := testIndexer(t, q)
	ix.clone = func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
		return clone.Result{Dir: d, Commit: testCommit}, nil
	}
	ix.walk = func(context.Context, string, walk.Limits) ([]walk.File, error) { return nil, nil }
	ix.put = func(context.Context, models.Repo, []models.File) error { return nil }
	ix.putSpans = okPutSpans
	ix.graph, ix.putGraph = okGraph, okPutGraph
	ix.evict = func(context.Context, int, int) (int, error) { return 0, errors.New("postgres went away") }

	ix.runJob(context.Background(), aJob())

	if out := logged.String(); !strings.Contains(out, "evict") || !strings.Contains(out, "postgres went away") {
		t.Errorf("a failed eviction left no usable trace: %q", out)
	}
	if len(q.completed) != 1 {
		t.Errorf("the job did not stay completed: %v", q.completed)
	}
	if len(q.failed) != 0 {
		t.Errorf("a failed eviction failed the job: %+v", q.failed)
	}
}

// What was dropped is the one number an operator needs to see the corpus being
// bounded; a silent eviction is indistinguishable from none.
func TestAnEvictionThatDroppedReposSaysHowMany(t *testing.T) {
	q := &fakeQueue{}
	ix, logged := testIndexer(t, q)
	ix.clone = func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
		return clone.Result{Dir: d, Commit: testCommit}, nil
	}
	ix.walk = func(context.Context, string, walk.Limits) ([]walk.File, error) { return nil, nil }
	ix.put = func(context.Context, models.Repo, []models.File) error { return nil }
	ix.putSpans = okPutSpans
	ix.graph, ix.putGraph = okGraph, okPutGraph
	ix.evict = func(context.Context, int, int) (int, error) { return 3, nil }

	ix.runJob(context.Background(), aJob())

	if out := logged.String(); !strings.Contains(out, `"evicted":3`) {
		t.Errorf("the eviction count is not in the log: %q", out)
	}
}

// Measured before this was fixed: github.com/octocat/Spoon-Knife and
// github.com/OctoCat/spoon-knife indexed to two repos rows at the same commit
// and six file rows for three files, and burned two eviction slots.
func TestCaseVariantRemotesIndexToOneRepo(t *testing.T) {
	index := func(remote string) models.Repo {
		ix, _ := testIndexer(t, &fakeQueue{})
		ix.clone = func(_ context.Context, r, _, d string, _ clone.Limits) (clone.Result, error) {
			// The submitted spelling is what is cloned; the forge decides the
			// display case and we do not get to rewrite it.
			if r != remote {
				t.Errorf("cloned %q, want the submitted %q", r, remote)
			}
			writeCheckout(t, d, map[string]string{"main.go": "package main\n"})
			return clone.Result{Dir: d, Commit: testCommit, Bytes: 1}, nil
		}
		ix.walk = func(context.Context, string, walk.Limits) ([]walk.File, error) {
			return []walk.File{{Path: "main.go", Lang: "go", Lines: 1, Bytes: 1}}, nil
		}
		var got models.Repo
		ix.put = func(_ context.Context, r models.Repo, _ []models.File) error {
			got = r
			return nil
		}
		ix.putSpans = okPutSpans
		ix.graph, ix.putGraph = okGraph, okPutGraph
		ix.evict = func(context.Context, int, int) (int, error) { return 0, nil }
		ix.runJob(context.Background(), jobs.Job{
			ID: "j", Remote: remote, Ref: "main", Status: jobs.StatusLeased, Attempts: 1,
		})
		return got
	}

	a := index("https://github.com/octocat/Spoon-Knife")
	b := index("https://github.com/OctoCat/spoon-knife")
	if a.ID != b.ID {
		t.Fatalf("one repository got two ids: %s and %s", a.ID, b.ID)
	}
	if a.Remote == b.Remote {
		t.Fatalf("both rows carry %q; the submitted spelling was rewritten", a.Remote)
	}
}

// The fixture repository the wiring tests index. Six files on purpose: one
// file hides an ordering or a batching fault, and a repository of nothing but
// parseable Go never takes the window fallback, so neither shape would tell a
// wired pipeline from a half-wired one.
var fixtureRepo = map[string]string{
	"a.go":       "package p\n\n// F does a thing.\nfunc F() {}\n",
	"b/util.go":  "package b\n\n// Helper explains itself.\nfunc Helper() int { return 1 }\n",
	"broken.go":  "package p\n\nfunc (\n",
	"README.md":  "# title\n\nsome prose\n",
	"logo.png":   "\x89PNG\r\n\x1a\n\x00\xff",
	"corrupt.go": "package p\n\xff\x00\n",
}

// recorder is what one hermetic job wrote: the rows, the spans, the order of
// the two writes, and the deadline each stage was handed.
type recorder struct {
	*fakeQueue
	logged    *bytes.Buffer
	files     []models.File
	spans     []store.EmbeddedSpan
	model     string
	dim       int
	order     []string
	deadlines map[string]time.Time
	// The graph the stage wrote, and the policy it handed the type-checker.
	syms   []models.Symbol
	edges  []models.Edge
	policy symbols.Policy
	// dropped is every repo id the terminal-failure cleanup was asked to
	// remove, in order.
	dropped []string
}

func (r *recorder) failReason() string {
	if len(r.failed) == 0 {
		return ""
	}
	return r.failed[0].reason
}

// spansByPath maps each path to its spans as "kind:symbol", which is what
// distinguishes the AST arm from the window arm row by row.
func (r *recorder) spansByPath() map[string][]string {
	out := map[string][]string{}
	for _, s := range r.spans {
		out[s.Path] = append(out[s.Path], string(s.Kind)+":"+s.Symbol)
	}
	return out
}

// recordingEmbedder notes the deadline it was called under, and can block on
// it: the embedder is the slowest stage, so it is where a per-stage deadline
// would be worth the most extra time.
type recordingEmbedder struct {
	embed.Embedder
	rec   *recorder
	block bool
}

func (e recordingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if d, ok := ctx.Deadline(); ok {
		e.rec.deadlines["embed"] = d
	}
	if e.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return e.Embedder.Embed(ctx, texts)
}

type fixture struct {
	files map[string]string
	block bool
	// resolved and stats are what the substituted type-checker answers, so a
	// test can say which call sites were named without forking a compiler.
	resolved    map[symbols.Key]symbols.Target
	stats       symbols.Stats
	graphErr    error
	forbidGraph bool
}

type fixtureOpt func(t *testing.T, f *fixture)

func withFiles(files map[string]string) fixtureOpt {
	return func(_ *testing.T, f *fixture) { f.files = files }
}

func stripping() fixtureOpt {
	return func(t *testing.T, _ *fixture) { t.Setenv("STRIP_DOC_COMMENTS", "true") }
}

func windowed() fixtureOpt {
	return func(t *testing.T, _ *fixture) { t.Setenv("CHUNK_STRATEGY", "window") }
}

func withBlockingEmbedder() fixtureOpt {
	return func(_ *testing.T, f *fixture) { f.block = true }
}

// withResolver is the type-checker's answer: which call sites it named, and
// why it stopped where it did.
func withResolver(resolved map[symbols.Key]symbols.Target, stats symbols.Stats) fixtureOpt {
	return func(_ *testing.T, f *fixture) { f.resolved, f.stats = resolved, stats }
}

func withGraphWriteError(err error) fixtureOpt {
	return func(_ *testing.T, f *fixture) { f.graphErr = err }
}

// withForbiddenResolver is the strong form of "the stage did not run":
// asserting that every edge is syntactic also passes when the resolver ran and
// failed. It fails the test where it is called rather than panicking, so the
// kill is an assertion and not an incident.
func withForbiddenResolver() fixtureOpt {
	return func(_ *testing.T, f *fixture) { f.forbidGraph = true }
}

func typechecking(on bool) fixtureOpt {
	return func(t *testing.T, _ *fixture) { t.Setenv("TYPECHECK", strconv.FormatBool(on)) }
}

// fakeIndexer drives a whole job with no network, no git and no Postgres: a
// clone that writes the fixture on disk, the real walk over it, the real
// chunker and the fake embedder, and a recorder in place of the two writes.
//
// The chunk knobs and the embedder come from chunkOptions, stripDocs and
// newEmbedder rather than from literals here, so a wrong default is a failing
// test rather than something only production would show.
func fakeIndexer(t *testing.T, opts ...fixtureOpt) (*indexer, *recorder) {
	t.Helper()
	f := &fixture{files: fixtureRepo, stats: symbols.Stats{Reason: symbols.ReasonOK}}
	for _, o := range opts {
		o(t, f)
	}
	t.Setenv("EMBED_PROVIDER", "fake")

	q := &fakeQueue{}
	ix, logged := testIndexer(t, q)
	rec := &recorder{fakeQueue: q, logged: logged, deadlines: map[string]time.Time{}}

	var err error
	if ix.opt, err = chunkOptions(); err != nil {
		t.Fatal(err)
	}
	// Through limitsFrom, as the chunk knobs are: a test that set TYPECHECK
	// by hand would be testing a worker production never builds.
	lim, err := limitsFrom()
	if err != nil {
		t.Fatal(err)
	}
	ix.lim.typecheck = lim.typecheck
	if ix.strip, err = stripDocs(); err != nil {
		t.Fatal(err)
	}
	base, err := newEmbedder(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ix.emb = recordingEmbedder{Embedder: base, rec: rec, block: f.block}

	ix.clone = func(ctx context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
		if dl, ok := ctx.Deadline(); ok {
			rec.deadlines["clone"] = dl
		}
		writeCheckout(t, d, f.files)
		return clone.Result{Dir: d, Commit: testCommit, Bytes: 1}, nil
	}
	ix.walk = func(ctx context.Context, root string, lim walk.Limits) ([]walk.File, error) {
		if dl, ok := ctx.Deadline(); ok {
			rec.deadlines["walk"] = dl
		}
		return walk.Files(ctx, root, lim)
	}
	ix.put = func(ctx context.Context, _ models.Repo, files []models.File) error {
		if dl, ok := ctx.Deadline(); ok {
			rec.deadlines["put"] = dl
		}
		rec.files, rec.order = files, append(rec.order, "put")
		return nil
	}
	ix.putSpans = func(ctx context.Context, _ string, spans []store.EmbeddedSpan, model string, dim int) error {
		if dl, ok := ctx.Deadline(); ok {
			rec.deadlines["putSpans"] = dl
		}
		rec.spans, rec.model, rec.dim = spans, model, dim
		rec.order = append(rec.order, "putSpans")
		return nil
	}
	ix.graph = func(ctx context.Context, p symbols.Policy) (map[symbols.Key]symbols.Target, symbols.Stats) {
		if f.forbidGraph {
			t.Error("the type-checker ran with TYPECHECK=false")
			return nil, symbols.Stats{Reason: symbols.ReasonLoadError}
		}
		if dl, ok := ctx.Deadline(); ok {
			rec.deadlines["graph"] = dl
		}
		rec.policy = p
		return f.resolved, f.stats
	}
	ix.putGraph = func(ctx context.Context, _ string, syms []models.Symbol, edges []models.Edge) error {
		if dl, ok := ctx.Deadline(); ok {
			rec.deadlines["putGraph"] = dl
		}
		rec.syms, rec.edges = syms, edges
		rec.order = append(rec.order, "putGraph")
		return f.graphErr
	}
	ix.evict = func(context.Context, int, int) (int, error) { return 0, nil }
	ix.dropRepo = func(_ context.Context, id string) (bool, error) {
		rec.dropped = append(rec.dropped, id)
		return true, nil
	}
	return ix, rec
}

// The default arm is AST, and every file that is not parseable Go still gets
// windowed. Shipping the baseline arm as production would be a silent
// downgrade of every answer, and nothing about the output would look wrong.
func TestAJobIndexesEachFileWithTheArmItsContentEarns(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.runJob(context.Background(), aJob())

	if rec.failReason() != "" {
		t.Fatalf("the job failed: %s", rec.failReason())
	}
	want := map[string][]string{
		"a.go":      {"func:F"},
		"b/util.go": {"func:Helper"},
		// Neither parses as Go, so both take the window fallback — and both
		// are kind=file, which is why nothing may read the arm off the kind.
		"broken.go": {"file:"},
		"README.md": {"file:"},
	}
	got := rec.spansByPath()
	if len(got) != len(want) {
		t.Fatalf("spans came from %v, want exactly %v", got, want)
	}
	for path, kinds := range want {
		if strings.Join(got[path], ",") != strings.Join(kinds, ",") {
			t.Errorf("%s produced %v, want %v", path, got[path], kinds)
		}
	}
}

// Files exist before spans reference them, and both before the graph:
// spans.file_id, symbols.file_id and symbols.span_id are foreign keys, so the
// wrong order is a failed insert against a real database. The graph is last
// for a second reason too — the span it links to by containment has to have a
// range before there is anything to contain.
func TestFilesAreWrittenBeforeSpansAndBothBeforeTheGraph(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.runJob(context.Background(), aJob())

	if want := []string{"put", "putSpans", "putGraph"}; !slices.Equal(rec.order, want) {
		t.Fatalf("call order %v, want %v", rec.order, want)
	}
	// Every span names a file row that was in that write.
	rows := map[string]bool{}
	for _, f := range rec.files {
		rows[f.ID] = true
	}
	for _, s := range rec.spans {
		if !rows[s.FileID] {
			t.Fatalf("span %s (%s) names file_id %s, which no row carries", s.ID, s.Path, s.FileID)
		}
	}
}

// Production keeps doc comments — they are the best signal a span has — and
// only the eval corpus strips them (spec §9). A default that stripped would
// quietly degrade the shipped index; one that never stripped would build an
// eval corpus holding the prose its own questions came from.
func TestDocCommentsAreKeptByDefaultAndStrippedWhenAsked(t *testing.T) {
	one := map[string]string{"a.go": fixtureRepo["a.go"]}

	ix, rec := fakeIndexer(t, withFiles(one))
	ix.runJob(context.Background(), aJob())
	if len(rec.spans) != 1 {
		t.Fatalf("want one span, got %d", len(rec.spans))
	}
	if !strings.Contains(rec.spans[0].Text, "F does a thing") {
		t.Fatalf("the production span lost its doc comment:\n%s", rec.spans[0].Text)
	}
	if rec.spans[0].StartLine != 3 || rec.spans[0].EndLine != 4 {
		t.Fatalf("production range is %d..%d, want 3..4 — the doc comment is part of the span",
			rec.spans[0].StartLine, rec.spans[0].EndLine)
	}

	ix, rec = fakeIndexer(t, withFiles(one), stripping())
	ix.runJob(context.Background(), aJob())
	if len(rec.spans) != 1 {
		t.Fatalf("want one span, got %d", len(rec.spans))
	}
	if strings.Contains(rec.spans[0].Text, "F does a thing") {
		t.Fatalf("the stripped span kept its doc comment:\n%s", rec.spans[0].Text)
	}
	if !strings.Contains(rec.spans[0].Text, "func F() {}") {
		t.Fatalf("the stripped span lost its code:\n%s", rec.spans[0].Text)
	}
	// Blanked, not deleted, so no line of code moves: func F is on line 4 in
	// both corpora. The span starts a line later only because the declaration
	// no longer has a doc comment to start at — measured, not predicted; the
	// first version of this assertion expected 3..4 and was wrong.
	if rec.spans[0].StartLine != 4 || rec.spans[0].EndLine != 4 {
		t.Fatalf("stripped range is %d..%d, want 4..4", rec.spans[0].StartLine, rec.spans[0].EndLine)
	}
}

// Go that will not parse cannot be stripped. Windowing it unstripped would put
// the prose the eval's questions came from into the corpus built to exclude it.
func TestAFileThatCannotBeStrippedContributesNoSpans(t *testing.T) {
	files := map[string]string{"broken.go": "package p\n\n// Doc prose here.\nfunc (\n"}

	ix, rec := fakeIndexer(t, withFiles(files))
	ix.runJob(context.Background(), aJob())
	if len(rec.spans) == 0 {
		t.Fatal("unstripped, an unparseable file is windowed and has spans")
	}

	ix, rec = fakeIndexer(t, withFiles(files), stripping())
	ix.runJob(context.Background(), aJob())
	for _, s := range rec.spans {
		t.Fatalf("stripping could not run on %s, yet it was indexed:\n%s", s.Path, s.Text)
	}
	if len(rec.files) != 1 {
		t.Fatalf("want the file row kept, got %d rows", len(rec.files))
	}
	if len(rec.completed) != 1 {
		t.Fatalf("one unparseable file failed the job: %+v", rec.failed)
	}
}

// A repository holding a PNG indexes. Without the guard the span insert is
// refused by Postgres for the whole transaction and one file loses the job.
func TestUnindexableFilesGetNoSpansButStillGetFileRows(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.runJob(context.Background(), aJob())

	if len(rec.files) != len(fixtureRepo) {
		t.Fatalf("got %d file rows, want %d", len(rec.files), len(fixtureRepo))
	}
	for _, s := range rec.spans {
		if s.Path == "logo.png" || s.Path == "corrupt.go" {
			t.Fatalf("%s was chunked: %q", s.Path, s.Text)
		}
	}
	// corrupt.go is the discriminating half: walk classifies it as Go, so only
	// the content check keeps it out.
	var sawCorrupt bool
	for _, f := range rec.files {
		if f.Path == "corrupt.go" {
			sawCorrupt = f.Lang == "go"
		}
	}
	if !sawCorrupt {
		t.Fatal("corrupt.go is not classified as Go, so this fixture no longer tests the content check")
	}
}

// Spec §3: files.blob is git's content hash, filled here because the chunk
// pass already holds the bytes.
func TestFileRowsCarryTheBlobHash(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.runJob(context.Background(), aJob())

	for _, f := range rec.files {
		want := store.BlobHash([]byte(fixtureRepo[f.Path]))
		if f.Blob != want {
			t.Errorf("%s: blob %q, want %q", f.Path, f.Blob, want)
		}
	}
}

// Spec §6: one deadline for the whole job, not a fresh one per stage. Each
// stage records the instant it was handed; a per-stage budget gives the later
// stages a later one.
func TestEveryStageSharesTheOneJobDeadline(t *testing.T) {
	ix, rec := fakeIndexer(t)
	start := time.Now()
	ix.runJob(context.Background(), aJob())

	stages := []string{"clone", "walk", "embed", "put", "putSpans", "graph"}
	for _, s := range stages {
		if rec.deadlines[s].IsZero() {
			t.Fatalf("%s ran with no deadline at all", s)
		}
		// JOB_DEADLINE_SECONDS is the budget, not some multiple of it. The
		// second of slack is for the microseconds between start and the
		// context being derived.
		if got := rec.deadlines[s].Sub(start); got > ix.lim.clone.Deadline+time.Second {
			t.Errorf("%s was given %s, more than the job's %s", s, got, ix.lim.clone.Deadline)
		}
	}
	for _, s := range stages[1:] {
		if !rec.deadlines[s].Equal(rec.deadlines["clone"]) {
			t.Errorf("%s got deadline %s, the clone got %s: that is a fresh budget per stage",
				s, rec.deadlines[s], rec.deadlines["clone"])
		}
	}
	// And the one write that is deliberately not on it. The type-check spends
	// the job's remaining budget by design, so writing its result on the same
	// context would make a slow type-check fail the job — the outcome spec:196
	// says is expected rather than exceptional. It is bounded, just not by the
	// deadline it was told to spend.
	if rec.deadlines["putGraph"].Equal(rec.deadlines["clone"]) {
		t.Errorf("the graph write ran on the job's own budget (%s): the type-check spends it",
			rec.deadlines["clone"])
	}
	if rec.deadlines["putGraph"].IsZero() {
		t.Error("the graph write ran with no deadline at all")
	}
}

// And the deadline is enforced, not merely carried. Falsifiable by
// construction: the parent outlives the job budget twentyfold, so a job that
// ignores its own deadline fails on the elapsed time rather than hanging.
func TestTheJobDeadlineReachesTheSlowestStage(t *testing.T) {
	ix, rec := fakeIndexer(t, withBlockingEmbedder())
	ix.lim.clone.Deadline = 100 * time.Millisecond
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	ix.runJob(parent, aJob())
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("the job ran for %v; the 100ms job deadline did not reach the embedder", elapsed)
	}
	if rec.failReason() == "" {
		t.Fatal("a job that ran out of time was not recorded as failed")
	}
}

// The failure of a timed-out job is written with a context that is not the one
// that just expired, or the job stays leased until its lease runs out and the
// reason never reaches the API (spec §10).
func TestATimedOutJobIsRecordedFailed(t *testing.T) {
	ix, rec := fakeIndexer(t, withBlockingEmbedder())
	ix.lim.clone.Deadline = 50 * time.Millisecond
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ix.runJob(parent, aJob())

	if len(rec.failedCtx) != 1 {
		t.Fatalf("want one recorded failure, got %+v", rec.failed)
	}
	if rec.failedCtx[0] != nil {
		t.Fatalf("Fail was called with an expired context (%v), so the write would not land", rec.failedCtx[0])
	}
	if !strings.Contains(rec.failReason(), "deadline") {
		t.Fatalf("the failure reason does not say what happened: %q", rec.failReason())
	}
}

// The completion is the same kind of write. A job whose work finished just
// inside its deadline would otherwise be marked done on an expired context:
// the rows are in, the row stays 'leased', and another worker redoes it.
func TestACompletionIsNotWrittenOnTheJobsExpiredContext(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.lim.clone.Deadline = 50 * time.Millisecond
	slow := ix.putSpans
	ix.putSpans = func(ctx context.Context, repo string, spans []store.EmbeddedSpan, model string, dim int) error {
		// The write lands, and the job's budget is gone by the time it returns.
		<-ctx.Done()
		return slow(context.Background(), repo, spans, model, dim)
	}
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ix.runJob(parent, aJob())

	if len(rec.completedCtx) != 1 {
		t.Fatalf("want one completion, got %v and failures %+v", rec.completed, rec.failed)
	}
	if rec.completedCtx[0] != nil {
		t.Fatalf("Complete was called with an expired context (%v): the job stays leased", rec.completedCtx[0])
	}
}

// Every span gets its own vector, and gets it in one piece: EMBED_BATCH splits
// the corpus, and a batch loop that stops early or misaligns files one span's
// text under another span's embedding.
func TestEverySpanCarriesItsOwnEmbedding(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.runJob(context.Background(), aJob())

	if len(rec.spans) <= ix.lim.embedBatch {
		t.Fatalf("%d spans at a batch of %d cannot show a batching fault", len(rec.spans), ix.lim.embedBatch)
	}
	fake := embed.NewFake(store.EmbeddingDim)
	for _, s := range rec.spans {
		want, err := fake.Embed(context.Background(), []string{s.Text})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(s.Embedding, want[0]) {
			t.Fatalf("span %s (%s:%d-%d) does not carry the vector of its own text",
				s.ID, s.Path, s.StartLine, s.EndLine)
		}
	}
}

// The chunk pass reads every file a second time. If that read does not go
// through walk.ReadRegular, a link swapped in between the walk and the read is
// followed — and every test in the walk package still passes, because they
// test the function and not this caller.
func TestTheSecondReadRefusesALinkSwappedInAfterTheWalk(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ix, rec := fakeIndexer(t, withFiles(map[string]string{"a.go": "package p\n"}))
	ix.clone = func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		// What the walk saw as a regular file is a symlink by the time the
		// chunker reads it.
		if err := os.Symlink(secret, filepath.Join(d, "bait.go")); err != nil {
			t.Fatal(err)
		}
		return clone.Result{Dir: d, Commit: testCommit, Bytes: 1}, nil
	}
	ix.walk = func(context.Context, string, walk.Limits) ([]walk.File, error) {
		return []walk.File{{Path: "bait.go", Lang: "go", Lines: 1, Bytes: 7}}, nil
	}

	ix.runJob(context.Background(), aJob())

	for _, s := range rec.spans {
		if strings.Contains(s.Text, "SECRET") {
			t.Fatalf("the link's target was indexed: %q", s.Text)
		}
	}
	if len(rec.spans) != 0 {
		t.Fatalf("a file that stopped being a regular file produced %d spans", len(rec.spans))
	}
	// The row stays, so the file count still describes what the walk saw, and
	// its blob is empty because those bytes were never read.
	if len(rec.files) != 1 || rec.files[0].Blob != "" {
		t.Fatalf("want one row with no blob, got %+v", rec.files)
	}
	if len(rec.completed) != 1 {
		t.Fatalf("one swapped file failed the whole job: %+v", rec.failed)
	}
}

// The embedder's width is checked at boot, not discovered at the first insert
// of the first job — by which time a clone, a walk and a whole embed pass have
// been spent.
func TestEmbedderConstructionRefusesTheWrongWidth(t *testing.T) {
	t.Setenv("EMBED_PROVIDER", "fake")
	t.Setenv("EMBED_DIM", "7")
	if _, err := newEmbedder(context.Background(), time.Minute); !errors.Is(err, store.ErrDimMismatch) {
		t.Fatalf("want ErrDimMismatch, got %v", err)
	}
}

// Every knob fails closed at boot, the way the caps do: config.GetInt reads a
// literal "0" as 0, and a zero window silently drops every fallback span.
func TestBadChunkOptionsRefuseToBoot(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"CHUNK_WINDOW_LINES", "0"},
		{"CHUNK_WINDOW_OVERLAP", "40"},
		{"CHUNK_WINDOW_OVERLAP", "-1"},
		{"CHUNK_MAX_DECL_LINES", "0"},
		{"CHUNK_STRATEGY", "asr"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := chunkOptions(); err == nil {
				t.Fatalf("%s=%s booted", tc.key, tc.value)
			}
		})
	}
	opt, err := chunkOptions()
	if err != nil {
		t.Fatalf("the defaults must boot: %v", err)
	}
	if opt != chunk.Defaults() {
		t.Fatalf("the defaults are %+v, want the chunker's own %+v", opt, chunk.Defaults())
	}
	t.Setenv("CHUNK_STRATEGY", "window")
	t.Setenv("CHUNK_WINDOW_LINES", "12")
	t.Setenv("CHUNK_WINDOW_OVERLAP", "3")
	t.Setenv("CHUNK_MAX_DECL_LINES", "99")
	opt, err = chunkOptions()
	if err != nil {
		t.Fatal(err)
	}
	want := chunk.Options{Strategy: chunk.StrategyWindow, WindowLines: 12, WindowOverlap: 3, MaxDeclLines: 99}
	if opt != want {
		t.Fatalf("want %+v, got %+v", want, opt)
	}
}

// STRIP_DOC_COMMENTS decides which corpus is being built, so a value it cannot
// read is a refusal rather than a false: "yes" would otherwise build an eval
// corpus holding the prose its questions came from and look like a good run.
func TestStripDocCommentsIsParsedNotGuessed(t *testing.T) {
	if got, err := stripDocs(); err != nil || got {
		t.Fatalf("production must keep doc comments: %v, %v", got, err)
	}
	for _, v := range []string{"true", "1", "TRUE"} {
		t.Setenv("STRIP_DOC_COMMENTS", v)
		if got, err := stripDocs(); err != nil || !got {
			t.Fatalf("%s: %v, %v", v, got, err)
		}
	}
	t.Setenv("STRIP_DOC_COMMENTS", "yes")
	if _, err := stripDocs(); err == nil {
		t.Fatal("a value that is not a boolean was read as one")
	}
}

// One vector per text, in order, is the Embedder's whole contract. A response
// short of the batch leaves the spans past the end with no vector at all, and
// PutSpans then refuses them for having 0 components — a complaint about the
// schema's width, naming a span, for a fault that belongs to the embedder.
func TestAShortEmbeddingBatchIsRefusedNamingTheEmbedder(t *testing.T) {
	ix, rec := fakeIndexer(t)
	ix.emb = shortEmbedder{Embedder: ix.emb}
	ix.runJob(context.Background(), aJob())

	if len(rec.spans) != 0 {
		t.Fatalf("spans were written from a short batch: %d", len(rec.spans))
	}
	if !strings.Contains(rec.failReason(), "vectors for") {
		t.Fatalf("the failure does not name the short batch: %q", rec.failReason())
	}
}

type shortEmbedder struct{ embed.Embedder }

func (e shortEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	v, err := e.Embedder.Embed(ctx, texts)
	if err != nil || len(v) == 0 {
		return v, err
	}
	return v[:len(v)-1], nil
}

// The queue writes are bounded as well as separate. A pool that has stopped
// answering must not hold the worker on a write that will never return — the
// job's context deliberately does not bound these, so if this does not,
// nothing does.
func TestAQueueWriteThatNeverReturnsIsBounded(t *testing.T) {
	restore := recordDeadline
	recordDeadline = 50 * time.Millisecond
	t.Cleanup(func() { recordDeadline = restore })

	ix, rec := fakeIndexer(t, withFiles(map[string]string{"a.go": "package p\n"}))
	rec.blockUntilDone = true
	// The parent outlives the bound twentyfold, so a write that is not bounded
	// fails on the elapsed time rather than hanging the suite.
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	ix.runJob(parent, aJob())
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("the completion held the worker for %v, past its %v bound", elapsed, recordDeadline)
	}
	if len(rec.completedCtx) != 1 || !errors.Is(rec.completedCtx[0], context.DeadlineExceeded) {
		t.Fatalf("want the write ended by its own bound, got %v", rec.completedCtx)
	}
}

// The eval's baseline arm over the eval's corpus, on the file shape that broke
// it: a package comment long enough that blanking it leaves a whole window
// with no word left in it (rs/zerolog's log.go is ~100 lines of one).
//
// Measured before chunk dropped those windows, with exactly these settings on
// the fake embedder spec §9 puts in CI: the job failed with "embedding spans
// 576-607: embed: text 19 has no tokens to hash" and the repository wrote 0
// rows, three attempts to terminal. The whole baseline arm of the eval was
// unbuildable, which is what P2 exists to make possible.
func TestTheEvalsWindowArmSurvivesAStrippedPackageComment(t *testing.T) {
	var doc strings.Builder
	for range 60 { // past one 40-line window, as zerolog's package comment is
		doc.WriteString("// prose the golden set's questions are generated from.\n")
	}
	ix, rec := fakeIndexer(t, windowed(), stripping(), withFiles(map[string]string{
		"log.go": doc.String() + "package p\n\nfunc F() int { return 1 }\n",
	}))
	// testIndexer's 1KB cap is for snippets; this fixture is a file, and one
	// the walk skipped for size would produce no spans for the wrong reason.
	ix.lim.walk.MaxFileBytes = 1 << 16
	ix.runJob(context.Background(), aJob())

	if r := rec.failReason(); r != "" {
		t.Fatalf("the job failed instead of indexing: %s", r)
	}
	if len(rec.spans) == 0 {
		t.Fatal("the stripped file produced no spans at all")
	}
	for _, s := range rec.spans {
		if !strings.ContainsFunc(s.Text, func(r rune) bool {
			return unicode.IsLetter(r) || unicode.IsDigit(r)
		}) {
			t.Errorf("span %s:%d-%d has nothing to embed:\n%q", s.Path, s.StartLine, s.EndLine, s.Text)
		}
	}
	// One window is lost: lines 1-40 are all inside the blanked comment, and
	// the next one reaches the code. Counted rather than dropped quietly — a
	// silent drop is how a corpus shrinks with nothing to read about it.
	if out := rec.logged.String(); !strings.Contains(out, `"tokenless":1`) {
		t.Errorf("the dropped-chunk count is not in the log: %q", out)
	}
}

// pollCounter drives run() for a fixed number of polls: fakeQueue calls stop
// each time it is asked for a job it has not got, so a counter there is the
// loop's iteration count. after runs at the poll it names, which is how the
// clock moves between phases.
func pollCounter(cancel func(), stopAt int, after map[int]func()) func() {
	polls := 0
	return func() {
		polls++
		if f, ok := after[polls]; ok {
			f()
		}
		if polls >= stopAt {
			cancel()
		}
	}
}

// The loop polls every POLL_SECONDS and the window the sweep enforces is a
// week, so a DELETE per poll would be a scan thousands of times more often than
// anything can change its answer. The interval check is the whole point.
//
// A fake clock, because the alternative is a wall-clock wait: with a real one
// this test is either a sleep or a flake, and rule 6 rules both out.
func TestTheSweepRunsAtMostOncePerInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ix *indexer
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	q := &fakeQueue{clock: func() time.Time { return now }}
	q.stop = pollCounter(cancel, 8, map[int]func(){
		// Just short of the interval on poll 4, so polls 5 and 6 still must not
		// sweep; over it on poll 6, so poll 7 must.
		4: func() { now = now.Add(ix.lim.sweepEvery - time.Second) },
		6: func() { now = now.Add(time.Second) },
	})
	ix, _ = testIndexer(t, q)
	ix.now = func() time.Time { return now }

	done := make(chan struct{})
	go func() { defer close(done); ix.run(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after its context was cancelled")
	}

	if len(q.leases) != 8 {
		t.Fatalf("the fixture drove %d polls, want 8", len(q.leases))
	}
	if len(q.sweptFor) != 2 {
		t.Fatalf("swept %d times in 8 polls spanning one interval boundary, want 2 (at %v)", len(q.sweptFor), q.sweptWhen)
	}
	// The second sweep is the one after the boundary, not a second one inside
	// the first interval.
	if !q.sweptWhen[1].Equal(q.sweptWhen[0].Add(ix.lim.sweepEvery)) {
		t.Errorf("swept at %v then %v, want the second an interval later", q.sweptWhen[0], q.sweptWhen[1])
	}
	for i, keepFor := range q.sweptFor {
		if keepFor != ix.lim.jobHistory {
			t.Errorf("sweep %d asked to keep %v, want JOB_HISTORY_HOURS of %v", i, keepFor, ix.lim.jobHistory)
		}
	}
}

// Job history is not what this worker is for. A sweep that cannot run must not
// cost the queue a poll — and must still respect its interval, or a database
// that is down turns into a DELETE attempt every two seconds.
func TestSweepFailureDoesNotStopLeasing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	q := &fakeQueue{sweepErr: errors.New("postgres went away"), clock: func() time.Time { return now }}
	q.stop = pollCounter(cancel, 4, nil)
	ix, logged := testIndexer(t, q)
	ix.now = func() time.Time { return now }

	done := make(chan struct{})
	go func() { defer close(done); ix.run(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a failing sweep stopped the lease loop")
	}

	if len(q.leases) != 4 {
		t.Errorf("leased %d times after a failing sweep, want 4", len(q.leases))
	}
	if len(q.sweptFor) != 1 {
		t.Errorf("a failing sweep ran %d times inside one interval, want 1", len(q.sweptFor))
	}
	if out := logged.String(); !strings.Contains(out, "job sweep failed") || !strings.Contains(out, "postgres went away") {
		t.Errorf("a failed sweep left no usable trace: %q", out)
	}
}

// The repo id is hash(key, commit) and only this binary ever saw the commit, so
// the job row is where a caller learns what their submission produced. It must
// be the id the same job wrote its rows under, not merely some id.
func TestACompletedJobNamesTheRepoItIndexed(t *testing.T) {
	q := &fakeQueue{}
	ix, _ := testIndexer(t, q)
	ix.clone = func(_ context.Context, _, _, d string, _ clone.Limits) (clone.Result, error) {
		writeCheckout(t, d, map[string]string{"main.go": mainGo})
		return clone.Result{Dir: d, Commit: testCommit, Bytes: 1}, nil
	}
	ix.walk = func(context.Context, string, walk.Limits) ([]walk.File, error) {
		return []walk.File{{Path: "main.go", Lang: "go", Lines: 3, Bytes: 40}}, nil
	}
	var written string
	ix.put = func(_ context.Context, r models.Repo, _ []models.File) error {
		written = r.ID
		return nil
	}
	ix.putSpans = okPutSpans
	ix.graph, ix.putGraph = okGraph, okPutGraph
	ix.evict = func(context.Context, int, int) (int, error) { return 0, nil }

	ix.runJob(context.Background(), aJob())

	want := store.RepoID("github.com/a/b", testCommit)
	if written != want {
		t.Fatalf("the fixture indexed %q, want %q", written, want)
	}
	if len(q.repoIDs) != 1 || q.repoIDs[0] != want {
		t.Errorf("the completed job names %v, want [%s]", q.repoIDs, want)
	}
}
