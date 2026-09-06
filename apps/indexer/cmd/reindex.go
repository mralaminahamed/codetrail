package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// reindexKnobs reads the two incremental-re-index switches.
//
// Both exist so the counterfactual is runnable: REINDEX_REUSE=false is what
// makes "reuse changes nothing in the rows" testable at all, exactly as
// TYPECHECK=false made P4's edge-count equality testable.
//
// Parsed, never compared against "true". The house rule, stated three times in
// this codebase: a knob an operator believes is in force and is not is the
// shape of bug this project has already shipped.
func reindexKnobs() (skipClone, reuse bool, err error) {
	if skipClone, err = boolKnob("REINDEX_SKIP_CLONE", "true"); err != nil {
		return false, false, err
	}
	if reuse, err = boolKnob("REINDEX_REUSE", "true"); err != nil {
		return false, false, err
	}
	return skipClone, reuse, nil
}

func boolKnob(key, def string) (bool, error) {
	v := config.Get(key, def)
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", key, v)
	}
	return b, nil
}

// fastPath reports whether this commit is already indexed in a form this worker
// would produce, so the job can complete without cloning anything.
//
// THREE conditions, not one, and the third is the one that is easy to miss.
//
//   - The repo row exists. Obvious, and not sufficient.
//   - Its span count is non-zero. A previous job could have completed with an
//     empty corpus — a repository of non-Go files, or one whose files all
//     vanished between the walk and the read — and serving that as "already
//     indexed" makes the emptiness PERMANENT, because the repair is the
//     re-index that just did nothing.
//   - Its spans carry THIS WORKER'S embed_model and embed_dim. RepoID =
//     hash(remote.Key, commit) contains no model and no dimension, so nothing
//     else in the system stops a re-index after an EMBED_MODEL change from
//     being a no-op — and with REINDEX_SKIP_CLONE defaulting to true, that is
//     the DEFAULT behaviour. An operator who changes the model and re-submits
//     every repository would get `done` for all of them in seconds, with the
//     old vectors still in place, and rag.ErrModelMismatch would then fire on
//     every query forever. This is the invariant store.CheckDim protects at
//     boot, one layer down.
//
// Any read failing is a miss rather than a job failure: the fast path is an
// optimisation, and an optimisation that can fail a job is a new failure mode
// for every job.
func (ix *indexer) fastPath(ctx context.Context, l zerolog.Logger, repoID string) bool {
	if _, err := ix.getRepo(ctx, repoID); err != nil {
		return false
	}
	n, err := ix.countSpans(ctx, repoID)
	if err != nil || n == 0 {
		if err != nil {
			l.Warn().Err(err).Msg("could not count spans; cloning")
		}
		return false
	}
	model, dim, err := ix.spanEmbedder(ctx, repoID)
	if err != nil {
		l.Warn().Err(err).Msg("could not read the corpus embedder; cloning")
		return false
	}
	if model != ix.emb.Model() || dim != ix.emb.Dim() {
		l.Info().Str("corpus_model", model).Int("corpus_dim", dim).
			Str("worker_model", ix.emb.Model()).Int("worker_dim", ix.emb.Dim()).
			Msg("the corpus was embedded by another model; cloning and re-embedding")
		return false
	}
	return true
}

// reuse fills in every span whose exact text has already been embedded by this
// exact embedder, and returns how many it filled.
//
// Keyed on the DIGEST, never on the span id: a span id contains the repo id,
// which contains the commit, so a span-id-keyed reuse could never hit across
// commits — which is every case this exists for — while still hitting within
// one commit's retry, so it would look like it worked.
//
// The safety argument is one equality, and it is verified rather than assumed:
// index() computes digest := store.Digest(c.Text) and embedAll builds the
// embedder's batch from sp.Text — the SAME string. So a digest is a content hash
// of exactly the bytes handed to Embed, and an embedding is a pure function of
// (text, model, dim).
//
// EVERY REUSED VECTOR IS CHECKED FOR ITS WIDTH before it is accepted, and NOT
// for its digest — which is a correction to the design this task was written
// against, made because the round showed the digest check could not fire.
//
// The plan asked for `store.Digest(sp.Text) == sp.Digest` on the grounds that it
// pins "the map key the store answered on is the digest we asked with". It does
// not: the lookup is `have[sp.Digest]`, so a Go map already guarantees that, and
// the equality itself is true by construction — index() sets both from the same
// string two dozen lines earlier. Measured: deleting the check entirely left
// every test passing, because the fixture that was supposed to catch it answers
// under a wrong key and so never reaches the check at all.
//
// The width IS reachable and defends the invariant that matters. spans.embed_dim
// is a plain INTEGER with no CHECK tying it to the vector(768) column, so a
// corrupt row can exist; lending its vector would put two vector spaces in one
// corpus, "where they would rank nonsense confidently and nothing about the
// query would look wrong". store.EmbeddingsByDigest refuses it on the way out
// and this refuses it on the way in — the same invariant store.CheckDim
// protects at boot, defended on both sides of one call.
//
// A failed read is not a failed job. Reuse is an optimisation and failing a job
// over one turns an optimisation into a new failure mode for every job: the
// error is logged, every span counts as embedded, and the job proceeds exactly
// as it would have before this existed.
func (ix *indexer) reuse(ctx context.Context, l zerolog.Logger, spans []store.EmbeddedSpan) int {
	if !ix.lim.reuse || len(spans) == 0 {
		return 0
	}
	seen := make(map[string]bool, len(spans))
	digests := make([]string, 0, len(spans))
	for _, sp := range spans {
		if !seen[sp.Digest] {
			seen[sp.Digest] = true
			digests = append(digests, sp.Digest)
		}
	}
	have, err := ix.embeddings(ctx, digests, ix.emb.Model(), ix.emb.Dim())
	if err != nil {
		l.Warn().Err(err).Msg("the reuse read failed; embedding everything")
		return 0
	}
	n := 0
	for i := range spans {
		v, ok := have[spans[i].Digest]
		if !ok {
			continue
		}
		// A LOOKUP, not a consumption: two spans with identical text share one
		// digest — a repeated helper inside one commit — and the second must
		// read the same entry rather than find it missing.
		if len(v) != ix.emb.Dim() {
			l.Error().Str("span", spans[i].ID).Int("components", len(v)).Int("want", ix.emb.Dim()).
				Msg("a reused vector is the wrong width; embedding it instead")
			continue
		}
		spans[i].Embedding = v
		n++
	}
	return n
}

// changedFiles is how many of this commit's files differ from the previous
// commit's, by git blob hash.
//
// Counted from the BLOBS and not from the file count: a repository where every
// file changed and none was added has an identical count, and a count-derived
// number would read 0 for exactly the case an operator most wants to see. This
// is the one job files.blob has.
//
// Returns -1 when there is no previous commit to compare against, so "the first
// index of this repository" is a distinguishable state rather than "nothing
// changed".
func changedFiles(now []models.File, before map[string]string) int {
	if len(before) == 0 {
		return -1
	}
	n := 0
	for _, f := range now {
		if b, ok := before[f.Path]; !ok || b != f.Blob {
			n++
		}
	}
	return n
}

// countReuse records how one job's spans were filled.
func countReuse(reused, total int) {
	metrics.CountReuse("reused", reused)
	metrics.CountReuse("embedded", total-reused)
}
