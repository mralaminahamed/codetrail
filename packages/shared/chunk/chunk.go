// Package chunk cuts one file into the retrievable regions a span is made of.
//
// Chunking takes a Strategy, not just a fallback. The eval (spec §9) compares
// AST spans against fixed windows over the *whole* corpus, so "window" has to
// be a thing a caller can ask for, not only what happens when the parser
// fails. Without that the baseline arm cannot be built and the headline
// comparison has nothing to compare against.
package chunk

import (
	"fmt"
	"unicode"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

type Strategy string

const (
	StrategyAST    Strategy = "ast"
	StrategyWindow Strategy = "window"
)

// Options are line-valued, not byte-valued. A span is cited by line range, so
// a byte window would have to account for lines anyway to name its range, and
// its boundaries would land mid-line where no citation can point.
type Options struct {
	Strategy      Strategy
	WindowLines   int
	WindowOverlap int
	// MaxDeclLines is where a declaration stops being one unit. Over it, the
	// declaration is sub-windowed: one generated 3,000-line file must not
	// become a single useless span (spec §5).
	MaxDeclLines int
}

// Chunk is one region of a file, before it is given an id and an embedding.
// StartLine and EndLine are 1-based and inclusive, as models.Span requires.
type Chunk struct {
	Kind      models.SpanKind
	Symbol    string
	StartLine int
	EndLine   int
	Text      string
}

func Defaults() Options {
	return Options{Strategy: StrategyAST, WindowLines: 40, WindowOverlap: 10, MaxDeclLines: 200}
}

func (o Options) Validate() error {
	switch o.Strategy {
	case StrategyAST, StrategyWindow:
	default:
		return fmt.Errorf("chunk: unknown strategy %q", o.Strategy)
	}
	if o.WindowLines <= 0 {
		return fmt.Errorf("chunk: WindowLines must be positive, got %d", o.WindowLines)
	}
	if o.WindowOverlap < 0 || o.WindowOverlap >= o.WindowLines {
		return fmt.Errorf("chunk: WindowOverlap must be in [0,%d), got %d", o.WindowLines, o.WindowOverlap)
	}
	if o.MaxDeclLines <= 0 {
		return fmt.Errorf("chunk: MaxDeclLines must be positive, got %d", o.MaxDeclLines)
	}
	return nil
}

// Chunks cuts src according to opt, and reports how many regions it dropped
// for having nothing to embed. filename is what the parser reports in an error
// and what decides whether the AST strategy tries at all; nothing is read from
// disk.
//
// It returns an error only for an invalid Options. A file that will not parse
// is not an error here — it is the window fallback, which is the whole point.
func Chunks(filename string, src []byte, opt Options) ([]Chunk, int, error) {
	if err := opt.Validate(); err != nil {
		return nil, 0, err
	}
	ls := splitLines(src)
	if len(ls) == 0 {
		return nil, 0, nil
	}
	var cs []Chunk
	if opt.Strategy == StrategyWindow {
		cs = windows(ls, 1, len(ls), opt)
	} else {
		cs = astChunks(filename, src, ls, opt)
	}
	kept, dropped := retrievable(cs)
	return kept, dropped, nil
}

// retrievable drops the chunks with nothing to embed and says how many.
//
// Here rather than in either strategy, and here rather than in the indexer:
// "a span holds something retrievable" is what this package emits, so both
// eval arms (spec §9) drop the same regions by construction — an AST arm that
// kept a sub-window the baseline arm dropped would bias the one comparison §9
// exists to make — and no caller has to remember. chunk knows nothing about
// embedders, but it does not have to: a region with no word in it is not a
// retrievable region under anyone's definition.
func retrievable(cs []Chunk) ([]Chunk, int) {
	kept := cs[:0]
	for _, c := range cs {
		if hasTokens(c.Text) {
			kept = append(kept, c)
		}
	}
	return kept, len(cs) - len(kept)
}

// hasTokens reports whether text holds anything an embedder can turn into a
// vector: one letter or digit anywhere is enough.
//
// Measured, on the eval's own configuration — CHUNK_STRATEGY=window,
// STRIP_DOC_COMMENTS=true over rs/zerolog: blanking log.go's ~100-line package
// comment leaves three windows that are entirely blank lines, embed.Fake
// refuses a text it can hash no token from, and the whole job fails with 0
// repos and 0 spans written. Dropped rather than embedded: Ollama does answer
// a blank text with a unit vector, but '[0,0,0]'::vector <=> '[1,2,3]'::vector
// is NaN in pgvector, so any zero-ish fallback ranks unpredictably instead of
// failing loudly, and there is nothing in the window to retrieve either way.
func hasTokens(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
