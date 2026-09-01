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

// Chunks cuts src according to opt. filename is what the parser reports in an
// error and what decides whether the AST strategy tries at all; nothing is
// read from disk.
//
// It returns an error only for an invalid Options. A file that will not parse
// is not an error here — it is the window fallback, which is the whole point.
func Chunks(filename string, src []byte, opt Options) ([]Chunk, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	ls := splitLines(src)
	if len(ls) == 0 {
		return nil, nil
	}
	if opt.Strategy == StrategyWindow {
		return windows(ls, 1, len(ls), opt), nil
	}
	return astChunks(filename, src, ls, opt), nil
}
