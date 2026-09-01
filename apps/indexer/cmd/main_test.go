package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/clone"
	"github.com/mralaminahamed/codetrail/apps/indexer/internal/walk"
	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
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

	leases    []string
	leaseFor  []time.Duration
	completed []string
	failed    []failCall
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

func (q *fakeQueue) Complete(_ context.Context, id, worker string) error {
	q.completed = append(q.completed, id+"@"+worker)
	return q.completeErr
}

func (q *fakeQueue) Fail(_ context.Context, id, worker, reason string, max int) error {
	q.failed = append(q.failed, failCall{id, worker, reason, max})
	return q.failErr
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
		},
		clone: func(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
			t.Error("clone ran when it should not have")
			return clone.Result{}, errors.New("unexpected clone")
		},
		walk: func(string, walk.Limits) ([]walk.File, error) {
			t.Error("walk ran when it should not have")
			return nil, errors.New("unexpected walk")
		},
		put: func(context.Context, models.Repo, []models.File) error {
			t.Error("put ran when it should not have")
			return errors.New("unexpected put")
		},
	}
	return ix, &logged
}

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func aJob() jobs.Job {
	return jobs.Job{ID: "job1", Remote: "https://github.com/a/b", Ref: "main", Status: jobs.StatusLeased, Attempts: 1}
}

// The caps must be positive or clone.Limits and walk.Limits refuse per job:
// a knob set to "0" would fail every repository in the queue to terminal
// rather than fail one boot, and config.GetInt reads "0" as 0, not as unset.
func TestLimitsRefuseANonPositiveKnob(t *testing.T) {
	for _, key := range []string{
		"MAX_REPO_BYTES", "JOB_DEADLINE_SECONDS", "MAX_REPO_FILES",
		"MAX_FILE_BYTES", "MAX_ATTEMPTS", "POLL_SECONDS",
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

// Every knob has to reach the cap it names. A crossed pair is silent: the
// worker boots, clones and indexes, with the wrong two numbers in force.
func TestLimitsAreWiredToTheCapsTheyName(t *testing.T) {
	t.Setenv("MAX_REPO_BYTES", "11")
	t.Setenv("JOB_DEADLINE_SECONDS", "22")
	t.Setenv("MAX_REPO_FILES", "33")
	t.Setenv("MAX_FILE_BYTES", "44")
	t.Setenv("MAX_ATTEMPTS", "55")
	t.Setenv("POLL_SECONDS", "66")
	lim, err := limitsFrom()
	if err != nil {
		t.Fatal(err)
	}
	want := limits{
		clone: clone.Limits{MaxBytes: 11, Deadline: 22 * time.Second},
		walk:  walk.Limits{MaxFiles: 33, MaxFileBytes: 44},
		tries: 55,
		poll:  66 * time.Second,
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
		return clone.Result{Dir: d, Commit: testCommit, Bytes: 4096}, nil
	}
	ix.walk = func(root string, lim walk.Limits) ([]walk.File, error) {
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

	ix.runJob(context.Background(), job)

	repoID := store.RepoID(job.Remote, testCommit)
	wantRepo := models.Repo{ID: repoID, Remote: job.Remote, Ref: job.Ref, Commit: testCommit, SizeBytes: 4096}
	if gotRepo != wantRepo {
		t.Errorf("repo: want %+v, got %+v", wantRepo, gotRepo)
	}
	want := []models.File{
		{ID: store.FileID(repoID, "main.go"), RepoID: repoID, Path: "main.go", Lang: "go", Lines: 3},
		{ID: store.FileID(repoID, "a/b.md"), RepoID: repoID, Path: "a/b.md", Lang: "markdown", Lines: 1},
	}
	if len(gotFiles) != len(want) {
		t.Fatalf("want %d files, got %d", len(want), len(gotFiles))
	}
	for i := range want {
		if gotFiles[i] != want[i] {
			t.Errorf("file %d: want %+v, got %+v", i, want[i], gotFiles[i])
		}
	}
	if len(q.completed) != 1 || q.completed[0] != job.ID+"@"+testWorker {
		t.Errorf("want the job completed as this worker, got %v", q.completed)
	}
	if len(q.failed) != 0 {
		t.Errorf("nothing failed, yet: %v", q.failed)
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
		return clone.Result{Dir: d, Commit: testCommit}, os.WriteFile(filepath.Join(d, "sub", "f.go"), []byte("x"), 0o600)
	}
	okWalk := func(string, walk.Limits) ([]walk.File, error) {
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
			ix.walk = func(string, walk.Limits) ([]walk.File, error) { return nil, errors.New("walk exploded") }
		}, true},
		{"write fails", func(ix *indexer) {
			ix.clone, ix.walk = okClone, okWalk
			ix.put = func(context.Context, models.Repo, []models.File) error { return errors.New("write exploded") }
		}, true},
		{"everything works", func(ix *indexer) {
			ix.clone, ix.walk = okClone, okWalk
			ix.put = func(context.Context, models.Repo, []models.File) error { return nil }
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
			ix.walk = func(string, walk.Limits) ([]walk.File, error) { return nil, nil }
			ix.put = func(context.Context, models.Repo, []models.File) error { return nil }

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
