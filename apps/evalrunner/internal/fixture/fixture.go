// Package fixture builds an eval corpus from a checkout on disk, for the live
// suites.
//
// It is a second derivation of apps/indexer/cmd's index loop, and that is a
// real cost stated rather than buried: the alternative is a live suite that
// needs a job queue, a git clone and a sandbox before it can build a two-arm
// corpus to check two arms against. **Nothing measured is built here.** Every
// artefact under docs/eval/runs comes from the shipped indexer binary, and
// spec §9's harness verifies a corpus rather than building one (Open question
// 4).
//
// What can drift is shared rather than copied: source.Read walks with
// walk.Files and filters with walk.Indexable, walk.Lines counts the lines,
// store.BlobHash hashes the blob, chunk.StripDocs strips and chunk.Chunks
// chunks, and store.SpanID keys the row. walk.Lines is on that list because it
// was NOT: this file held a correct second copy of a counter the indexer had
// wrong, so the two derivations disagreed about how long a file is — which is
// exactly the drift the source-binding check exists to catch. What is reimplemented is the loop that puts them in order, and
// corpus.Verify is run against the result by the same tests, so a loop that
// drifted from the indexer's would have to drift in a way that still satisfies
// every check the product makes.
package fixture

import (
	"context"
	"fmt"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/source"
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/walk"
)

// Options is one arm: which checkout, at which commit, chunked how, stripped
// or not.
type Options struct {
	Remote string
	Ref    string
	Commit string
	Root   string
	Chunk  chunk.Options
	Strip  bool
	Emb    embed.Embedder
}

// Counters are the per-job figures the indexer logs and nothing stores. The
// eval's artefact copies them out of that log line; here they come back
// directly, because a live fixture that could not see tokenless would not
// notice a corpus quietly shrinking.
type Counters struct {
	Vanished     int `json:"vanished"`
	Unstrippable int `json:"unstrippable"`
	Tokenless    int `json:"tokenless"`
}

// Index writes one arm's corpus and returns its repo id.
func Index(ctx context.Context, s *store.Store, o Options) (string, Counters, error) {
	var c Counters
	if err := o.Chunk.Validate(); err != nil {
		return "", c, err
	}
	repoID := store.RepoID(o.Remote, o.Commit)
	files, err := source.Read(ctx, o.Root, source.Limits())
	if err != nil {
		return "", c, err
	}

	rows := make([]models.File, 0, len(files))
	var spans []store.EmbeddedSpan
	for _, f := range files {
		row := models.File{
			ID: store.FileID(repoID, f.Path), RepoID: repoID, Path: f.Path,
			Lang: f.Lang, Lines: walk.Lines(f.Body),
		}
		if !f.Read {
			c.Vanished++
			rows = append(rows, row)
			continue
		}
		// Before stripping, exactly as the indexer hashes it — the two arms
		// must agree on this value or corpus.Verify refuses them.
		row.Blob = store.BlobHash(f.Body)
		rows = append(rows, row)
		if !f.Indexable() {
			continue
		}
		src := f.Body
		if o.Strip {
			stripped, serr := chunk.StripDocs(f.Path, f.Body)
			if serr != nil {
				c.Unstrippable++
				continue
			}
			src = stripped
		}
		cs, blank, cerr := chunk.Chunks(f.Path, src, o.Chunk)
		if cerr != nil {
			return "", c, fmt.Errorf("chunking %s: %w", f.Path, cerr)
		}
		c.Tokenless += blank
		for _, ch := range cs {
			digest := store.Digest(ch.Text)
			spans = append(spans, store.EmbeddedSpan{Span: models.Span{
				ID: store.SpanID(repoID, f.Path, ch.StartLine, ch.EndLine, digest), RepoID: repoID,
				FileID: row.ID, Path: f.Path, Kind: ch.Kind, Symbol: ch.Symbol,
				StartLine: ch.StartLine, EndLine: ch.EndLine, Text: ch.Text, Digest: digest,
			}})
		}
	}

	texts := make([]string, 0, len(spans))
	for _, sp := range spans {
		texts = append(texts, sp.Text)
	}
	vecs, err := o.Emb.Embed(ctx, texts)
	if err != nil {
		return "", c, err
	}
	if len(vecs) != len(spans) {
		return "", c, fmt.Errorf("fixture: embedder answered %d vectors for %d spans", len(vecs), len(spans))
	}
	for i := range spans {
		spans[i].Embedding = vecs[i]
	}

	if err := s.PutRepo(ctx, models.Repo{
		ID: repoID, Remote: o.Remote, Ref: o.Ref, Commit: o.Commit,
	}, rows); err != nil {
		return "", c, err
	}
	if err := s.PutSpans(ctx, repoID, spans, o.Emb.Model(), o.Emb.Dim()); err != nil {
		return "", c, err
	}
	return repoID, c, nil
}
