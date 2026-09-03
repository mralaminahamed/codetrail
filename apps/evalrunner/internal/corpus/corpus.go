// Package corpus makes "both arms index the same corpus at the same commit" a
// property rather than a promise.
//
// Nothing in the schema records which arm produced a corpus, or whether it was
// stripped: kind=file does not say, and embed_model records only the embedder.
// So an operator who points both arms at one database, or forgets
// STRIP_DOC_COMMENTS on one of two indexer runs, gets a complete run with
// plausible numbers and nothing anywhere looks wrong. These checks run before
// any retrieval happens and refuse rather than warn.
package corpus

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/mralaminahamed/codetrail/packages/shared/testdb"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
)

// Reader is the store surface these checks need.
//
// An interface, not *store.Store: store.Store is struct{ pool *pgxpool.Pool }
// with the pool unexported, so a concrete store cannot be faked and every
// hermetic test below would silently become a live one. P3 hit the same wall
// and gave rag.Searcher the same seam.
type Reader interface {
	Commit(ctx context.Context, repoID string) (string, error)
	FileBlobs(ctx context.Context, repoID string) (map[string]string, error)
	SpanRanges(ctx context.Context, repoID string) ([]metric.Span, error)
	SpanTexts(ctx context.Context, repoID, after string, limit int) ([]TextRow, string, error)
}

// TextRow is one span's text, as the leakage probe reads it.
type TextRow struct {
	SpanID string `json:"span_id"`
	Path   string `json:"path"`
	Text   string `json:"text"`
}

// Arm is one side of the comparison: a name, the DSN it lives behind, the
// repository row inside it, and a way to read them.
//
// Verify derives the database name itself rather than taking one. Two DSNs
// differing only in sslmode or a pool parameter name one database, so a
// caller that compared the strings would admit two arms that overwrite each
// other — and P2 proved live that the second arm then silently replaces the
// first. The DSN never reaches the artefact; Database() is what does.
type Arm struct {
	Name   string
	DSN    string
	RepoID string
	Read   Reader
}

// Database is the database this arm's DSN names, which is what the artefact
// records: a DSN carries a password.
//
// testdb.Name is host-blind — verified, it parses the URL and returns the path
// — so two databases with the same name on different hosts compare equal and
// are refused. That is a false refusal and it is the safe direction: it
// refuses a legal configuration rather than admitting an illegal one, and the
// operator can rename. It also returns "" for a DSN it cannot parse, which
// makes two unparseable DSNs compare equal — also a refusal, also safe, and
// store.New would have failed on them first.
func (a Arm) Database() string { return testdb.Name(a.DSN) }

// Report is what Verify establishes, for the artefact.
type Report struct {
	Commit        string         `json:"commit"`
	Files         map[string]int `json:"files"`
	Spans         map[string]int `json:"spans"`
	SharedSpanIDs int            `json:"shared_span_ids"`
}

// Leak is one golden question found inside one indexed span.
//
// Path is here because a count cannot be acted on: it is what tells an
// operator whether the leak is a missed .go file or a README.md, which are
// different problems with different fixes.
type Leak struct {
	CaseID string `json:"case_id"`
	SpanID string `json:"span_id"`
	Path   string `json:"path"`
}

// Every check refuses with its own sentinel naming which rule failed, matching
// spec:258's rule for admission rejections. A warning on a run that writes a
// JSON file full of numbers is a warning nobody reads.
var (
	ErrSameDatabase    = errors.New("corpus: both arms name one database")
	ErrCommitMismatch  = errors.New("corpus: the arms are at different commits")
	ErrFileSetMismatch = errors.New("corpus: the arms disagree about a file's bytes")
	ErrIdenticalArms   = errors.New("corpus: the arms have identical span ids; nothing to compare")
	ErrSourceMismatch  = errors.New("corpus: the source the questions came from is not what was indexed")
	ErrLeaked          = errors.New("corpus: a golden question's prose is in the indexed corpus")
	ErrTooLarge        = errors.New("corpus: too many spans to probe")
)

// Verify refuses every way the two arms can fail to be one corpus.
//
// src is path to blob for the checkout the questions were generated from,
// hashed with store.BlobHash — the indexer's own function, which runs before
// stripping. Until this check existed nothing said where that checkout came
// from: the runner's flags named two DSNs and a repo id, and an operator
// pointing -src at the wrong commit gets every case's range naming lines the
// corpus does not have. The leakage probe does not fire on that, because a
// question generated from the wrong file is prose that appears nowhere.
//
// The order is most specific first. Two DSNs naming one database also produce
// identical span sets, so a looser order would report ErrSameDatabase as
// ErrIdenticalArms and send an operator to fix the wrong thing.
func Verify(ctx context.Context, a, b Arm, src map[string]string) (Report, error) {
	var rep Report

	if a.Database() == b.Database() {
		return rep, fmt.Errorf("%w: %s", ErrSameDatabase, a.Database())
	}

	ac, err := a.Read.Commit(ctx, a.RepoID)
	if err != nil {
		return rep, fmt.Errorf("corpus: reading %s's commit: %w", a.Name, err)
	}
	bc, err := b.Read.Commit(ctx, b.RepoID)
	if err != nil {
		return rep, fmt.Errorf("corpus: reading %s's commit: %w", b.Name, err)
	}
	if ac != bc {
		return rep, fmt.Errorf("%w: %s is at %s and %s is at %s", ErrCommitMismatch, a.Name, ac, b.Name, bc)
	}
	rep.Commit = ac

	af, err := a.Read.FileBlobs(ctx, a.RepoID)
	if err != nil {
		return rep, fmt.Errorf("corpus: reading %s's files: %w", a.Name, err)
	}
	bf, err := b.Read.FileBlobs(ctx, b.RepoID)
	if err != nil {
		return rep, fmt.Errorf("corpus: reading %s's files: %w", b.Name, err)
	}
	rep.Files = map[string]int{a.Name: len(af), b.Name: len(bf)}
	if err := sameBytes(a.Name, af, b.Name, bf, ErrFileSetMismatch); err != nil {
		return rep, err
	}
	// The same evidence, pointed at the third party nobody was checking.
	if err := sameBytes("-src", src, a.Name, af, ErrSourceMismatch); err != nil {
		return rep, err
	}
	if err := sameBytes("-src", src, b.Name, bf, ErrSourceMismatch); err != nil {
		return rep, err
	}

	as, err := a.Read.SpanRanges(ctx, a.RepoID)
	if err != nil {
		return rep, fmt.Errorf("corpus: reading %s's spans: %w", a.Name, err)
	}
	bs, err := b.Read.SpanRanges(ctx, b.RepoID)
	if err != nil {
		return rep, fmt.Errorf("corpus: reading %s's spans: %w", b.Name, err)
	}
	rep.Spans = map[string]int{a.Name: len(as), b.Name: len(bs)}
	rep.SharedSpanIDs = shared(as, bs)
	// The id set, not the count: two arms can coincidentally produce the same
	// number of spans and cannot produce the same ids unless they chunked
	// identically. P2 measured three shared ids on the indexer's fixture repo,
	// so a nonzero intersection is normal and is reported rather than refused.
	if len(as) == len(bs) && rep.SharedSpanIDs == len(as) {
		return rep, fmt.Errorf("%w: %d spans each", ErrIdenticalArms, len(as))
	}
	return rep, nil
}

// sameBytes compares two path-to-blob maps and names the first path they
// disagree about, in a stable order.
//
// On (path, blob) and not on counts: "the same corpus" is the same bytes. A
// count comparison passes when one arm ran under a different MAX_FILE_BYTES,
// which drops a file's content while keeping its row.
func sameBytes(an string, a map[string]string, bn string, b map[string]string, sentinel error) error {
	paths := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, m := range []map[string]string{a, b} {
		for p := range m {
			if !seen[p] {
				seen[p], paths = true, append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		av, aok := a[p]
		bv, bok := b[p]
		switch {
		case !aok:
			return fmt.Errorf("%w: %s has %s and %s does not", sentinel, bn, p, an)
		case !bok:
			return fmt.Errorf("%w: %s has %s and %s does not", sentinel, an, p, bn)
		case av != bv:
			return fmt.Errorf("%w: %s is %s in %s and %s in %s", sentinel, p, av, an, bv, bn)
		}
	}
	return nil
}

func shared(a, b []metric.Span) int {
	ids := make(map[string]bool, len(a))
	for _, s := range a {
		ids[s.ID] = true
	}
	n := 0
	for _, s := range b {
		if ids[s.ID] {
			n++
		}
	}
	return n
}
