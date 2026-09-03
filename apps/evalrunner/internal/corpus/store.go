package corpus

import (
	"context"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// FromStore adapts the shipped store to Reader.
//
// A thin adapter rather than a second set of queries: the harness opening its
// own pgx connection beside the store would be a second place for a repo
// filter to be forgotten, which is the one mistake every read in this project
// is scoped to prevent.
func FromStore(s *store.Store) Reader { return storeReader{s} }

type storeReader struct{ s *store.Store }

func (r storeReader) Commit(ctx context.Context, repoID string) (string, error) {
	repo, err := r.s.GetRepo(ctx, repoID)
	return repo.Commit, err
}

func (r storeReader) FileBlobs(ctx context.Context, repoID string) (map[string]string, error) {
	return r.s.FileBlobs(ctx, repoID)
}

func (r storeReader) SpanRanges(ctx context.Context, repoID string) ([]metric.Span, error) {
	rs, err := r.s.SpanRanges(ctx, repoID)
	if err != nil {
		return nil, err
	}
	out := make([]metric.Span, 0, len(rs))
	for _, s := range rs {
		out = append(out, metric.Span{ID: s.ID, Path: s.Path, Start: s.StartLine, End: s.EndLine})
	}
	return out, nil
}

func (r storeReader) SpanTexts(ctx context.Context, repoID, after string, limit int) ([]TextRow, string, error) {
	ts, next, err := r.s.SpanTexts(ctx, repoID, after, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]TextRow, 0, len(ts))
	for _, t := range ts {
		out = append(out, TextRow{SpanID: t.ID, Path: t.Path, Text: t.Text})
	}
	return out, next, nil
}
