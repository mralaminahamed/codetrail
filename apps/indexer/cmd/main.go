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
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/logger"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/symbols"
)

// queue is the slice of jobs.Queue this worker uses. An interface so the loop
// and every failure path can be driven without a database.
type queue interface {
	Lease(ctx context.Context, worker string, d time.Duration) (jobs.Job, bool, error)
	Complete(ctx context.Context, id, worker, repoID string) error
	Fail(ctx context.Context, id, worker, reason string, maxAttempts int) error
	Sweep(ctx context.Context, keepFor time.Duration) (int, error)
}

type limits struct {
	clone clone.Limits
	walk  walk.Limits
	tries int
	poll  time.Duration
	// keepRepos is how many repositories the corpus is allowed to hold. Anyone
	// may submit one, so something has to bound it; see runJob.
	keepRepos int
	// keepTombstones bounds the evicted_repos table the same way, and for the
	// same reason: eviction is unbounded, so what records it has to be bounded.
	keepTombstones int
	// embedBatch is how many span texts go in one Embed call.
	embedBatch int
	// jobHistory is how long a terminal job stays readable, and sweepEvery how
	// often that is enforced; see sweepJobs.
	jobHistory time.Duration
	sweepEvery time.Duration
	// typecheck is the graph stage's kill switch and goProxy the module proxy
	// it may use; goBin is the go binary this process found at boot, empty
	// when there is none. See logToolchain for why an empty one still boots.
	typecheck bool
	goBin     string
	goProxy   string
}

// indexer is the worker: a queue, the steps of a job, and the caps.
// clone, walk, put, putSpans and evict are fields rather than direct calls, so
// a test can drive a job with no network, no git and no Postgres.
type indexer struct {
	log      zerolog.Logger
	q        queue
	clone    func(ctx context.Context, remote, ref, dir string, lim clone.Limits) (clone.Result, error)
	walk     func(ctx context.Context, root string, lim walk.Limits) ([]walk.File, error)
	put      func(ctx context.Context, r models.Repo, files []models.File) error
	putSpans func(ctx context.Context, repoID string, spans []store.EmbeddedSpan, model string, dim int) error
	// graph is the type-checker and putGraph the graph write, seams for the
	// same reason as the rest: the labelling is testable without forking a
	// compiler twice per assertion.
	graph    func(ctx context.Context, p symbols.Policy) (map[symbols.Key]symbols.Target, symbols.Stats)
	putGraph func(ctx context.Context, repoID string, syms []models.Symbol, edges []models.Edge) error
	evict    func(ctx context.Context, keep, keepTombstones int) (int, error)
	emb      embed.Embedder
	// opt and strip are the chunking decisions, read once at boot rather than
	// per job: see chunkOptions and stripDocs.
	opt   chunk.Options
	strip bool
	// hosts is the same allowlist the gateway admits against, applied again
	// here; see runJob.
	hosts admit.Policy

	// id is this process's name in the jobs table; see workerID.
	id      string
	scratch string
	lim     limits

	// now is the clock the sweep interval is measured on, a field so a test
	// can count sweeps without waiting an hour. lastSweep is zero at boot, so
	// the first pass of the loop sweeps.
	now       func() time.Time
	lastSweep time.Time
}

func main() {
	log := logger.New("indexer")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lim, err := limitsFrom()
	if err != nil {
		log.Fatal().Err(err).Msg("bad limits")
	}
	opt, err := chunkOptions()
	if err != nil {
		log.Fatal().Err(err).Msg("bad chunk options")
	}
	strip, err := stripDocs()
	if err != nil {
		log.Fatal().Err(err).Msg("bad strip setting")
	}
	// Before the database connection, so a width that cannot be written is a
	// refusal to boot rather than a job's worth of work discovering it.
	emb, err := newEmbedder(ctx, lim.clone.Deadline)
	if err != nil {
		log.Fatal().Err(err).Msg("bad embedder")
	}

	st, err := store.New(ctx, config.Get("DATABASE_URL", "postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable"))
	if err != nil {
		log.Fatal().Err(err).Msg("connect postgres")
	}
	defer st.Close()

	ix := &indexer{
		log:      log,
		q:        jobs.New(st.Pool()),
		clone:    clone.Run,
		walk:     walk.Files,
		put:      st.PutRepo,
		putSpans: st.PutSpans,
		graph:    symbols.Resolve,
		putGraph: st.PutGraph,
		evict:    st.Evict,
		emb:      emb,
		opt:      opt,
		strip:    strip,
		hosts:    admit.NewPolicy(allowedHosts()),
		id:       workerID(),
		scratch:  config.Get("SCRATCH_DIR", filepath.Join(os.TempDir(), "codetrail")),
		lim:      lim,
		now:      time.Now,
	}
	// At exit, so a worker asked to stop takes its checkouts with it. At boot
	// too: with a fresh id each start that normally finds nothing, and one
	// syscall is cheaper than depending on it having found nothing.
	ix.sweepHome()
	defer ix.sweepHome()

	ix.logToolchain()
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
	if err := removeScratch(ix.home()); err != nil {
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
// A value that is not a number at all is config.GetInt's own refusal, and that
// knob's range is not checked on top of it: MAX_REPO_FILES=2OOOO has no range
// to be outside of.
//
// KEEP_REPOS is here for the same reason and a worse consequence: Evict reads a
// keep of 0 as "keep nothing" and deletes every repo, so the one knob where a
// zero would silently destroy data is the one that must not boot with it.
func limitsFrom() (limits, error) {
	var err error
	get := func(key string, def int) int {
		n, e := config.GetInt(key, def)
		if e == nil && n <= 0 {
			e = fmt.Errorf("%s must be positive, got %d", key, n)
		}
		if e != nil && err == nil {
			err = e
		}
		return n
	}
	// The two graph knobs are read the same way and refused the same way: a
	// value that is not a boolean, and a proxy that would fetch from a host a
	// stranger's go.mod names, are both operator errors a job cannot fix. The
	// go binary is not a knob at all — its absence is a downgrade, not a
	// misconfiguration, so it is looked up and never refused.
	typecheck, terr := typecheckEnabled()
	if terr != nil && err == nil {
		err = terr
	}
	proxy, perr := typecheckProxy()
	if perr != nil && err == nil {
		err = perr
	}
	return limits{
		typecheck: typecheck,
		goBin:     goToolchain(),
		goProxy:   proxy,
		clone: clone.Limits{
			MaxBytes: int64(get("MAX_REPO_BYTES", 256<<20)),
			Deadline: time.Duration(get("JOB_DEADLINE_SECONDS", 600)) * time.Second,
		},
		walk: walk.Limits{
			MaxFiles:     get("MAX_REPO_FILES", 20000),
			MaxFileBytes: int64(get("MAX_FILE_BYTES", 1<<20)),
		},
		tries:          get("MAX_ATTEMPTS", 3),
		poll:           time.Duration(get("POLL_SECONDS", 2)) * time.Second,
		keepRepos:      get("KEEP_REPOS", 50),
		keepTombstones: get("KEEP_TOMBSTONES", 500),
		embedBatch:     get("EMBED_BATCH", 32),
		// One week. A caller may poll a job id for that long after it finishes,
		// which is a rule that can be stated; see jobs.Sweep for the two bounds
		// this is chosen over.
		jobHistory: time.Duration(get("JOB_HISTORY_HOURS", 168)) * time.Hour,
		sweepEvery: time.Duration(get("JOB_SWEEP_MINUTES", 60)) * time.Minute,
	}, err
}

// run leases and executes jobs until the process is asked to stop.
func (ix *indexer) run(ctx context.Context) {
	for ctx.Err() == nil {
		ix.sweepJobs(ctx)
		// A minute past the deadline the whole job shares, so the job is not
		// handed to a second worker while the first is still working it. The
		// spare minute is for the queue writes that follow the work; see
		// recordDeadline.
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

// sweepJobs ages terminal jobs out of the queue, at most once per
// JOB_SWEEP_MINUTES.
//
// Here rather than in the gateway: spec §2 says the gateway writes job rows and
// repos.last_queried_at and nothing else, and the indexer already owns terminal
// states. In the lease loop rather than a goroutine of its own, so a worker
// that stops leasing stops sweeping.
//
// The interval check is what keeps the DELETE off every poll: the loop polls
// every POLL_SECONDS and the window it enforces is a week, so sweeping per poll
// would be a scan a couple of thousand times more often than anything can
// change the answer.
func (ix *indexer) sweepJobs(ctx context.Context) {
	if ix.now().Sub(ix.lastSweep) < ix.lim.sweepEvery {
		return
	}
	ix.lastSweep = ix.now()
	n, err := ix.q.Sweep(ctx, ix.lim.jobHistory)
	if err != nil {
		// Logged and dropped. Retention is not what this worker is for, the
		// next interval tries again, and a failure here must not cost the queue
		// a poll.
		ix.log.Warn().Err(err).Msg("job sweep failed")
		return
	}
	if n > 0 {
		ix.log.Info().Int("swept", n).Str("older_than", ix.lim.jobHistory.String()).
			Msg("aged out terminal jobs")
	}
}

// runJob clones, walks, chunks, embeds and records one leased job.
//
// Every stage shares one deadline (spec §6), so a slow clone cannot buy itself
// extra time by failing into the embedder, and the lease taken in run outlasts
// it. The lease can still be lost — a stage that ignores its context, a process
// paused long enough — and that is survived rather than prevented: PutRepo and
// PutSpans are idempotent so two workers converge on the same rows, and
// Complete refuses for whoever no longer holds the lease.
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
	remote, err := ix.hosts.Check(job.Remote)
	if err != nil {
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
	// The go command's caches, beside the checkout rather than under it: it is
	// this job's alone, so nothing a stranger's build wrote outlives the job
	// and no two jobs share a cache one of them filled — but inside the
	// checkout, `go list ./...` would walk it and a fetched module would bring
	// a go.mod of its own into the tree being type-checked.
	goHome := dir + ".gohome"
	// Removed before and after, and both are load-bearing. Before: git clone
	// refuses a non-empty destination, so a checkout left by a crash would fail
	// every retry of this job on the leftover rather than on the repository.
	// After: a failed job that leaves its checkout behind fills the disk one
	// failure at a time, and clone.Run's own cleanup concedes it does not
	// survive a descendant that outlived the process-group kill.
	if err := errors.Join(removeScratch(dir), removeScratch(goHome)); err != nil {
		l.Error().Err(err).Msg("could not clear the scratch directory")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}
	defer removeScratch(dir)
	defer removeScratch(goHome)

	// One deadline for the whole job (spec §6): clone, read, chunk, embed and
	// write share it, so a slow clone cannot buy itself extra time by failing
	// into the embedder. A budget per stage hands each stage the whole number
	// again, and a job may then run for as many deadlines as it has stages —
	// with embedding, the slowest of them, free to spend a full one on its own.
	jobCtx, cancel := context.WithTimeout(ctx, ix.lim.clone.Deadline)
	defer cancel()

	res, err := ix.clone(jobCtx, remote.URL, job.Ref, dir, ix.lim.clone)
	if err != nil {
		l.Warn().Err(err).Msg("clone failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}
	files, err := ix.walk(jobCtx, res.Dir, ix.lim.walk)
	if err != nil {
		l.Warn().Err(err).Msg("walk failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}

	// The repo is keyed by the commit that was actually fetched, not by the ref
	// that was asked for: a branch moves, and a citation into "main" would mean
	// a different file next week. And by remote.Key rather than the row's
	// spelling, so two case-variant submissions are one repository.
	repoID := store.RepoID(remote.Key, res.Commit)
	rows, spans, parsed, err := ix.index(jobCtx, l, repoID, res.Dir, files)
	if err != nil {
		l.Warn().Err(err).Msg("indexing failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}
	repo := models.Repo{ID: repoID, Remote: job.Remote, Ref: job.Ref, Commit: res.Commit, SizeBytes: res.Bytes}
	if err := ix.put(jobCtx, repo, rows); err != nil {
		l.Error().Err(err).Msg("write failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}
	// After the files, not before: spans.file_id references files(id), so this
	// order is the foreign key's and not a preference.
	if err := ix.putSpans(jobCtx, repoID, spans, ix.emb.Model(), ix.emb.Dim()); err != nil {
		l.Error().Err(err).Msg("writing spans failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}

	// The writes that close the job out run on their own budget, derived from
	// the process and not from jobCtx: the job's deadline is what has just
	// expired on a job that ran out of time, and a write on it does not land —
	// leaving the row 'leased' with no reason on it until the lease runs out.
	done, cancelDone := context.WithTimeout(ctx, recordDeadline)
	defer cancelDone()

	// After putSpans, because a symbol's span_id references a row that has to
	// exist and the containment link is computed against the ranges the
	// chunker has just written. Before Complete, because the checkout goes
	// when this function returns and the type-checker reads it.
	if err := ix.runGraph(jobCtx, done, l, graphJob{
		repoID: repoID, root: res.Dir, home: goHome,
		parsed: parsed, files: rows, spans: spans,
	}); err != nil {
		l.Error().Err(err).Msg("writing the graph failed")
		ix.fail(ctx, l, job.ID, err.Error())
		return
	}

	if err := ix.q.Complete(done, job.ID, ix.id, repoID); err != nil {
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
	l.Info().Str("commit", res.Commit).Int("files", len(rows)).Int("spans", len(spans)).
		Int64("bytes", res.Bytes).Msg("indexed")

	// Here rather than on a timer: a successful index is the only thing that
	// grows the corpus, so it is the only moment the quota can be exceeded.
	//
	// A failure is logged and dropped. The rows are written and the lease is
	// released by now, so there is nothing left to retry, and the next
	// successful index evicts down to the same bound regardless of how far
	// over this one left it.
	if n, err := ix.evict(done, ix.lim.keepRepos, ix.lim.keepTombstones); err != nil {
		l.Warn().Err(err).Msg("evict failed")
	} else if n > 0 {
		l.Info().Int("evicted", n).Msg("evicted least recently queried repos")
	}
}

// index reads every walked file a second time, chunks it and embeds the
// chunks, returning the file rows, the spans ready to write, and what the
// graph pass read out of the same bytes.
//
// The second read goes through walk.ReadRegular rather than os.ReadFile. The
// walk counts lines and drops the bytes, so these bytes have to be read again,
// and a plain read would follow a symlink swapped in after the walk — the hole
// P1 closed — while every test in the walk package still passed, because they
// test the function and not this caller.
func (ix *indexer) index(ctx context.Context, l zerolog.Logger, repoID, root string, files []walk.File) ([]models.File, []store.EmbeddedSpan, []symbols.File, error) {
	rows := make([]models.File, 0, len(files))
	var spans []store.EmbeddedSpan
	var parsed []symbols.File
	var vanished, unstrippable, tokenless, unparsed int
	for _, f := range files {
		// The job's deadline reaches the read stage here. MAX_FILE_BYTES bounds
		// one read and MAX_REPO_FILES bounds how many there are, but neither
		// bounds how long they take, and the walk's own check is behind us.
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		row := models.File{ID: store.FileID(repoID, f.Path), RepoID: repoID, Path: f.Path, Lang: f.Lang, Lines: f.Lines}
		body, _, err := walk.ReadRegular(filepath.Join(root, filepath.FromSlash(f.Path)), ix.lim.walk.MaxFileBytes)
		if err != nil {
			if !errors.Is(err, walk.ErrSkipped) {
				return nil, nil, nil, fmt.Errorf("reading %s: %w", f.Path, err)
			}
			// Between the walk and now the entry stopped being an indexable
			// regular file — swapped for a link, or grown past the cap. The row
			// stays, so the file count still describes what the walk saw; its
			// blob is empty because these bytes were never read.
			vanished++
			rows = append(rows, row)
			continue
		}
		row.Blob = blobHash(body)
		rows = append(rows, row)
		if !indexable(f, body) {
			continue
		}
		// The graph pass parses the raw body, and never the stripped src
		// below. StripDocs keeps every line number and removes the prose
		// bytes, so the same call has different offsets in the two streams —
		// while go/packages keys its resolutions against the file on disk.
		// Stripped bytes here would make every lookup miss, with no error
		// anywhere and a resolution rate of zero that reads as a repository
		// with no in-repo calls. Pinned by
		// TestTheGraphPassParsesTheRawBodyAndNotTheStrippedSource, which
		// refuses to pass if stripping ever stops moving offsets.
		if f.Lang == "go" {
			pf, perr := symbols.Parse(f.Path, body)
			if perr != nil {
				// Same trade the chunker makes: a file that does not parse is
				// normal in a stranger's repository, and it costs its own
				// definitions rather than the job.
				unparsed++
			} else {
				parsed = append(parsed, pf)
			}
		}
		src := body
		if ix.strip {
			stripped, serr := chunk.StripDocs(f.Path, body)
			if serr != nil {
				// Go that will not parse cannot be stripped, and the corpus
				// this flag builds must not carry the prose its questions were
				// generated from. Windowing it unstripped would put that prose
				// in; failing the job would cost every other file's spans.
				unstrippable++
				continue
			}
			src = stripped
		}
		cs, blank, cerr := chunk.Chunks(f.Path, src, ix.opt)
		if cerr != nil {
			// Chunks errors only on an invalid Options, which chunkOptions
			// refused at boot. Returned rather than ignored: it would mean the
			// options changed under a running worker.
			return nil, nil, nil, fmt.Errorf("chunking %s: %w", f.Path, cerr)
		}
		tokenless += blank
		for _, c := range cs {
			digest := store.Digest(c.Text)
			spans = append(spans, store.EmbeddedSpan{Span: models.Span{
				ID: store.SpanID(repoID, f.Path, c.StartLine, c.EndLine, digest), RepoID: repoID,
				FileID: row.ID, Path: f.Path, Kind: c.Kind, Symbol: c.Symbol,
				StartLine: c.StartLine, EndLine: c.EndLine, Text: c.Text, Digest: digest,
			}})
		}
	}
	// Once per job, not once per file: a repository of generated code would
	// otherwise write a line per file into a log nobody can then read. Counted
	// rather than dropped quietly — tokenless in particular is how a corpus
	// shrinks without anyone noticing, since the job still succeeds.
	if vanished > 0 || unstrippable > 0 || tokenless > 0 || unparsed > 0 {
		l.Warn().Int("vanished", vanished).Int("unstrippable", unstrippable).
			Int("tokenless", tokenless).Int("unparsed", unparsed).
			Msg("some of this repository produced no spans")
	}
	if err := ix.embedAll(ctx, spans); err != nil {
		return nil, nil, nil, err
	}
	return rows, spans, parsed, nil
}

// embedAll fills in each span's vector, EMBED_BATCH texts per request.
//
// The Embedder contract is one vector per text in order, so a response of the
// wrong length is refused here: left alone, the spans past the end keep a nil
// embedding and PutSpans refuses them for having 0 components — a message
// about the schema's width, naming a span, for a fault that belongs to the
// embedder.
func (ix *indexer) embedAll(ctx context.Context, spans []store.EmbeddedSpan) error {
	for lo := 0; lo < len(spans); lo += ix.lim.embedBatch {
		hi := min(lo+ix.lim.embedBatch, len(spans))
		texts := make([]string, 0, hi-lo)
		for _, sp := range spans[lo:hi] {
			texts = append(texts, sp.Text)
		}
		vecs, err := ix.emb.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embedding spans %d-%d: %w", lo, hi-1, err)
		}
		if len(vecs) != len(texts) {
			return fmt.Errorf("embedding spans %d-%d: %s returned %d vectors for %d texts",
				lo, hi-1, ix.emb.Model(), len(vecs), len(texts))
		}
		for i := range vecs {
			spans[lo+i].Embedding = vecs[i]
		}
	}
	return nil
}

// recordDeadline bounds the queue writes that close a job out. A pool that has
// stopped answering must not hold the worker on a write forever: the job's own
// context is deliberately not what bounds these, so nothing else would.
//
// A var so a test can shorten it; nothing writes it in production.
var recordDeadline = 30 * time.Second

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
//
// ctx here is the process's, never the job's. A job that ran out of time is
// exactly the one whose failure has to be written, and writing it on the
// context that just expired writes nothing: the row stays 'leased' with no
// reason on it until the lease runs out (spec §10).
func (ix *indexer) recordFailure(ctx context.Context, l zerolog.Logger, id, reason string, maxAttempts int) {
	ctx, cancel := context.WithTimeout(ctx, recordDeadline)
	defer cancel()
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
