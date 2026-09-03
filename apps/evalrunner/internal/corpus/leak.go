package corpus

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
)

// PageSize is how many span texts Probe reads at a time.
//
// Measured: rs/zerolog's AST arm is 1,303 spans and about a megabyte, so 200
// rows is a few hundred kilobytes in flight and seven round trips. One query
// per case would be len(cases) round trips instead, and a whole-corpus read
// would be one allocation the size of the repository.
const PageSize = 200

// MaxSpans bounds the scan. A corpus over it is refused with a message saying
// so rather than being scanned to exhaustion — the probe reads every byte of
// text a repository has, and "it worked but took an hour" is a failure nothing
// asserts against.
const MaxSpans = 200_000

// Probe reports every golden question whose prose is findable in an indexed
// span, and refuses the corpus if there is one.
//
// It scans every span's text, not the gold span's and not the top hit's. The
// prose can be anywhere: a non-Go file — verified, chunk.StripDocs("NOTES.md",
// …) returns its input unchanged — a duplicated comment in a test file, or an
// in-body comment quoting the doc. Any of those makes the question findable,
// and the probe's job is to say the corpus is unusable rather than to say the
// gold span is clean.
//
// This is what makes "the corpus is stripped" checkable without a new column:
// an unstripped corpus fails it by construction, which is stronger than a flag
// nothing reads back.
func Probe(ctx context.Context, a Arm, cases []golden.Case) ([]Leak, error) {
	// Only the normalised questions are held; the span texts are read in
	// bounded pages and dropped.
	type needle struct{ caseID, q string }
	needles := make([]needle, 0, len(cases))
	for _, c := range cases {
		if q := golden.Normalise(c.Question); q != "" {
			needles = append(needles, needle{c.ID, q})
		}
	}

	var leaks []Leak
	var scanned int
	after := ""
	for {
		rows, next, err := a.Read.SpanTexts(ctx, a.RepoID, after, PageSize)
		if err != nil {
			return nil, fmt.Errorf("corpus: reading %s's span text: %w", a.Name, err)
		}
		for _, r := range rows {
			if scanned++; scanned > MaxSpans {
				return nil, fmt.Errorf("%w: %s has more than %d spans", ErrTooLarge, a.Name, MaxSpans)
			}
			text := golden.Normalise(r.Text)
			for _, n := range needles {
				if contains(text, n.q) {
					leaks = append(leaks, Leak{CaseID: n.caseID, SpanID: r.SpanID, Path: r.Path})
				}
			}
		}
		if next == "" {
			break
		}
		after = next
	}

	// Stable, so two runs over one corpus report the same first leak and the
	// artefact stays diffable.
	sort.Slice(leaks, func(i, j int) bool {
		if leaks[i].CaseID != leaks[j].CaseID {
			return leaks[i].CaseID < leaks[j].CaseID
		}
		return leaks[i].SpanID < leaks[j].SpanID
	})
	if len(leaks) > 0 {
		l := leaks[0]
		return leaks, fmt.Errorf("%w: case %q appears in span %s at %s (%d leaks); the corpus was not stripped",
			ErrLeaked, l.CaseID, l.SpanID, l.Path, len(leaks))
	}
	return nil, nil
}

// contains is substring containment over two already-normalised strings. Named
// so the comparison has one place to be wrong in: normalising the question and
// not the text is a probe that finds nothing and reports a clean corpus.
func contains(text, question string) bool {
	if question == "" {
		return false
	}
	return strings.Contains(text, question)
}
