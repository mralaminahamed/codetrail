// Command indexer leases indexing jobs and runs them.
//
// It is the only component that handles an untrusted URL, so it is a separate
// binary: the sandbox is then a deployment boundary rather than a promise.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/clone"
	"github.com/mralaminahamed/codetrail/apps/indexer/internal/walk"
	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/logger"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// queue is the slice of jobs.Queue this worker uses. An interface so the loop
// and every failure path can be driven without a database.
type queue interface {
	Lease(ctx context.Context, worker string, d time.Duration) (jobs.Job, bool, error)
	Complete(ctx context.Context, id, worker string) error
	Fail(ctx context.Context, id, worker, reason string, maxAttempts int) error
}

type limits struct {
	clone clone.Limits
	walk  walk.Limits
	tries int
	poll  time.Duration
	// keepRepos is how many repositories the corpus is allowed to hold. Anyone
	// may submit one, so something has to bound it; see runJob.
	keepRepos int
}

// indexer is the worker: a queue, the steps of a job, and the caps.
// clone, walk, put and evict are fields rather than direct calls, so a test can
// drive a job with no network, no git and no Postgres.
type indexer struct {
	log   zerolog.Logger
	q     queue
	clone func(ctx context.Context, remote, ref, dir string, lim clone.Limits) (clone.Result, error)
	walk  func(root string, lim walk.Limits) ([]walk.File, error)
	put   func(ctx context.Context, r models.Repo, files []models.File) error
	evict func(ctx context.Context, keep int) (int, error)
	// hosts is the same allowlist the gateway admits against, applied again
	// here; see runJob.
	hosts admit.Policy

	// id is this process's name in the jobs table; see workerID.
	id      string
	scratch string
	lim     limits
}

func main() {
	log := logger.New("indexer")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lim, err := limitsFrom()
	if err != nil {
		log.Fatal().Err(err).Msg("bad limits")
	}

	st, err := store.New(ctx, config.Get("DATABASE_URL", "postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable"))
	if err != nil {
		log.Fatal().Err(err).Msg("connect postgres")
	}
	defer st.Close()

	ix := &indexer{
		log:     log,
		q:       jobs.New(st.Pool()),
		clone:   clone.Run,
		walk:    walk.Files,
		put:     st.PutRepo,
		evict:   st.Evict,
		hosts:   admit.NewPolicy(allowedHosts()),
		id:      workerID(),
		scratch: config.Get("SCRATCH_DIR", filepath.Join(os.TempDir(), "codetrail")),
		lim:     lim,
	}
	// At exit, so a worker asked to stop takes its checkouts with it. At boot
	// too: with a fresh id each start that normally finds nothing, and one
	// syscall is cheaper than depending on it having found nothing.
	ix.sweepHome()
	defer ix.sweepHome()

	log.Info().Str("worker", ix.id).Msg("indexer up")
	ix.run(ctx)
}

// allowedHosts reads the exact-host allowlist the same way the gateway does:
// ALLOWED_HOSTS, comma-separated, replacing the default rather than extending
// it. Duplicated rather than shared because the two binaries are separate
// packages; if a third reader appears, this belongs in admit.
func allowedHosts() []string {
	return strings.Split(config.Get("ALLOWED_HOSTS", strings.Join(admit.DefaultHosts, ",")), ",")
}

// home is this worker's own scratch subtree. Per worker, because two indexers
// sharing one SCRATCH_DIR otherwise collide on the same job directory after a
// lease expiry: measured, the second worker's pre-clone RemoveAll destroyed the
// first's in-flight clone ("Unable to read current working directory") and the
// first's deferred RemoveAll then destroyed the second's, failing both on a
// healthy repository.
func (ix *indexer) home() string { return filepath.Join(ix.scratch, ix.id) }

// sweepHome removes this worker's subtree and nothing else. A peer's tree is
// left alone deliberately: nothing here distinguishes a crashed worker's
// directory from a live one's, and removing the wrong one is the collision
// above with extra steps. A worker killed hard therefore still leaks its tree.
func (ix *indexer) sweepHome() {
	if err := os.RemoveAll(ix.home()); err != nil {
		ix.log.Warn().Err(err).Str("dir", ix.home()).Msg("could not clear the worker's scratch tree")
	}
}

// workerID names this process in the jobs table. Complete and Fail authorise
// on it, so two indexers sharing an id can silently finish each other's jobs:
// the ownership check passes and nothing raises an error.
//
// It is generated rather than configured. HOSTNAME is unset in a plain shell
// and identical for two indexers on one host, and a value no code reads cannot
// be set to the same string twice. The hostname stays as a prefix so
// jobs.leased_by still says where the worker is.
func workerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "indexer"
	}
	var b [8]byte
	// crypto/rand.Read never returns an error; it crashes the program instead.
	_, _ = rand.Read(b[:])
	return host + "-" + hex.EncodeToString(b[:])
}

// limitsFrom reads the caps from the environment and refuses a non-positive
// one at boot.
//
// clone.Limits and walk.Limits both reject a zero rather than reading it as
// "unlimited", and config.GetInt returns 0 for the literal value "0" — so
// without this, MAX_REPO_FILES=0 would lease every job in the queue, fail each
// one on its caps, and burn them all to terminal. Failing one boot is the
// diagnosable version of that.
//
// KEEP_REPOS is here for the same reason and a worse consequence: Evict reads a
// keep of 0 as "keep nothing", so the typo that means "unlimited" everywhere
// else would delete the whole corpus after every successful index.
func limitsFrom() (limits, error) {
	var err error
	get := func(key string, def int) int {
		n := config.GetInt(key, def)
		if n <= 0 && err == nil {
			err = fmt.Errorf("%s must be positive, got %d", key, n)
		}
		return n
	}
	return limits{
		clone: clone.Limits{
			MaxBytes: int64(get("MAX_REPO_BYTES", 256<<20)),
			Deadline: time.Duration(get("JOB_DEADLINE_SECONDS", 600)) * time.Second,
		},
		walk: walk.Limits{
			MaxFiles:     get("MAX_REPO_FILES", 20000),
			MaxFileBytes: int64(get("MAX_FILE_BYTES", 1<<20)),
		},
		tries:     get("MAX_ATTEMPTS", 3),
		poll:      time.Duration(get("POLL_SECONDS", 2)) * time.Second,
		keepRepos: get("KEEP_REPOS", 50),
	}, err
}

// run leases and executes jobs until the process is asked to stop.
func (ix *indexer) run(ctx context.Context) {
	for ctx.Err() == nil {
		// A minute past the clone's own deadline, so the job is not handed to a
		// second worker while the first is still cloning it. It does not cover
		// the walk, which has no deadline of its own — see runJob.
		job, ok, err := ix.q.Lease(ctx, ix.id, ix.lim.clone.Deadline+time.Minute)
		if err != nil {
			ix.log.Error().Err(err).Msg("lease")
			sleep(ctx, ix.lim.poll)
			continue
		}
		if !ok {
			sleep(ctx, ix.lim.poll)
			continue
		}
		ix.runJob(ctx, job)
	}
}

// runJob clones, walks and records one leased job.
//
// The lease can expire under it: walk.Files has no deadline and clone's covers
// only the fetch, so a slow repository can be reclaimed mid-job. That is not
// corrected here, it is survived — PutRepo is idempotent so both workers
// converge on the same rows, and Complete refuses for whoever no longer holds
// the lease.
func (ix *indexer) runJob(ctx context.Context, job jobs.Job) {
	l := ix.log.With().Str("job", job.ID).Str("remote", job.Remote).Logger()

	// jobs.Fail's attempt cap only binds when someone calls Fail, and a job
	// that kills its indexer never does: the lease expires, the next worker
	// leases it, attempts climbs and status stays 'leased' forever. This worker
	// holds the lease now, so it is the one that can end that.
	if job.Attempts > ix.lim.tries {
		l.Warn().Int("attempts", job.Attempts).Msg("abandoned by earlier leases, failing")
		ix.failFinally(ctx, l, job.ID, fmt.Sprintf("abandoned after %d attempts", job.Attempts))
		return
	}
	// git clone --branch takes a ref name; a full commit hash there is fatal
	// ("Remote branch <sha> not found in upstream origin", measured against
	// git 2.43). The API's ref check accepts a hash, so this is where it is
	// refused — permanently, because no retry and no configuration change can
	// make that clone succeed.
	if isCommitSHA(job.Ref) {
		l.Warn().Str("ref", job.Ref).Msg("ref is a commit sha, which cannot be cloned")
		ix.failFinally(ctx, l, job.ID, "ref must be a branch or tag name: a commit sha cannot be cloned")
		return
	}

	// This binary's doc comment claims it is the component that handles the
	// untrusted URL. Checking the row rather than trusting it is what makes
	// that true: the gateway admits before enqueueing, but a row it did not
	// write, or an allowlist edited since it did, reaches git otherwise.
	if _, err := ix.hosts.Check(job.Remote); err != nil {
		l.Warn().Err(err).Msg("remote refused")
		// A malformed URL or a non-https scheme is a property of the string
		// and no retry or setting can make it clonable. An unlisted host can
		// become listed, by an operator editing ALLOWED_HOSTS, so that one
		// keeps its attempts.
		var ae *admit.Error
		if errors.As(err, &ae) && ae.Rule == admit.RuleHost {
			ix.fail(ctx, l, job.ID, err.Error())
		} else {
			ix.failFinally(ctx, l, job.ID, err.Error())
		}
		return
	}

	dir := filepath.Join(ix.home(), job.ID)
	// Removed before and after, and both are load-bearing. Before: git clone
	// refuses a non-empty destination, so a checkout left by a crash would fail
	// every retry of this job on the leftover rather than on the repository.
	// After: a failed job that leaves its checkout behind fills the disk one
	// failure at a time, and clone.Run's own cleanup concedes it does not
	// survive a descendant that outlived the process-group kill.
	if err := os.RemoveAll(dir); err != nil {
		l.Error().Err(err).Msg("could not clear the scratch directory")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}
	defer os.RemoveAll(dir)

	res, err := ix.clone(ctx, job.Remote, job.Ref, dir, ix.lim.clone)
	if err != nil {
		l.Warn().Err(err).Msg("clone failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}
	files, err := ix.walk(res.Dir, ix.lim.walk)
	if err != nil {
		l.Warn().Err(err).Msg("walk failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}

	// The repo is keyed by the commit that was actually fetched, not by the ref
	// that was asked for: a branch moves, and a citation into "main" would mean
	// a different file next week.
	repoID := store.RepoID(job.Remote, res.Commit)
	rows := make([]models.File, 0, len(files))
	for _, f := range files {
		rows = append(rows, models.File{
			ID: store.FileID(repoID, f.Path), RepoID: repoID,
			// Blob is git's content hash and stays empty in P1: nothing reads
			// it until P2 needs it for incremental re-indexing.
			Path: f.Path, Blob: "", Lang: f.Lang, Lines: f.Lines,
		})
	}
	repo := models.Repo{ID: repoID, Remote: job.Remote, Ref: job.Ref, Commit: res.Commit, SizeBytes: res.Bytes}
	if err := ix.put(ctx, repo, rows); err != nil {
		l.Error().Err(err).Msg("write failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}
	if err := ix.q.Complete(ctx, job.ID, ix.id); err != nil {
		// ErrNotLeased means this worker's lease expired and another indexer
		// took the job. Losing that race is normal; completing someone else's
		// job would not be. Anything else — a cancelled context under SIGTERM,
		// a dead connection — is a write that did not happen with the lease
		// still held, and saying "lease no longer held" there tells an operator
		// something the code has not established.
		if errors.Is(err, jobs.ErrNotLeased) {
			l.Warn().Err(err).Msg("could not complete: lease no longer held")
		} else {
			l.Warn().Err(err).Msg("could not complete: the job stays leased until it expires")
		}
		return
	}
	l.Info().Str("commit", res.Commit).Int("files", len(rows)).Int64("bytes", res.Bytes).Msg("indexed")

	// Here rather than on a timer: a successful index is the only thing that
	// grows the corpus, so it is the only moment the quota can be exceeded.
	//
	// A failure is logged and dropped. The rows are written and the lease is
	// released by now, so there is nothing left to retry, and the next
	// successful index evicts down to the same bound regardless of how far
	// over this one left it.
	if n, err := ix.evict(ctx, ix.lim.keepRepos); err != nil {
		l.Warn().Err(err).Msg("evict failed")
	} else if n > 0 {
		l.Info().Int("evicted", n).Msg("evicted least recently queried repos")
	}
}

// fail returns the job to the queue against its attempt budget.
func (ix *indexer) fail(ctx context.Context, l zerolog.Logger, id, reason string) {
	ix.recordFailure(ctx, l, id, reason, ix.lim.tries)
}

// failFinally ends the job now, whatever its attempt count. A cap of zero
// makes jobs.Fail take the terminal branch, because attempts is at least 1
// after a lease. It is for the refusals a retry cannot change.
func (ix *indexer) failFinally(ctx context.Context, l zerolog.Logger, id, reason string) {
	ix.recordFailure(ctx, l, id, reason, 0)
}

// recordFailure logs when the lease was already lost, rather than discarding
// the one signal that says this worker's verdict was not recorded.
func (ix *indexer) recordFailure(ctx context.Context, l zerolog.Logger, id, reason string, maxAttempts int) {
	err := ix.q.Fail(ctx, id, ix.id, reason, maxAttempts)
	switch {
	case err == nil:
	case errors.Is(err, jobs.ErrNotLeased):
		l.Warn().Err(err).Msg("could not record failure: lease no longer held")
	default:
		// See Complete: this one is a failed write, not a lost lease.
		l.Warn().Err(err).Msg("could not record failure: the job stays leased until it expires")
	}
}

// isCommitSHA reports whether ref is a full object name rather than a ref
// name: 40 hex digits for sha1, 64 for sha256.
//
// Only the full lengths, deliberately. A short hash is also unclonable, but
// "deadbeef" is a plausible branch name and refusing it would cost a real ref;
// a short hash instead fails the clone and spends the attempt budget. In the
// other direction a 40-character hex branch name is legal in git and is
// refused here — that trade is the one worth making, since nobody names a
// branch that way and everybody pastes a commit hash.
func isCommitSHA(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for _, r := range ref {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
