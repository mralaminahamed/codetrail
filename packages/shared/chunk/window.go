package chunk

import (
	"strings"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// splitLines returns src's lines without their terminators, and nothing at all
// for a file that is empty or only whitespace. Text is rebuilt from these, so
// a chunk's Text is by construction exactly the lines its range names.
func splitLines(src []byte) []string {
	if len(strings.TrimSpace(string(src))) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(src), "\n")
	return strings.Split(s, "\n")
}

// windows tiles [from,to] — 1-based inclusive, indices into ls — with fixed
// windows of opt.WindowLines striding by WindowLines-WindowOverlap.
//
// It is used for three things, which is why it takes a range rather than a
// file: the whole file under StrategyWindow, one unparseable file under
// StrategyAST, and one over-long declaration under either.
func windows(ls []string, from, to int, opt Options) []Chunk {
	stride := opt.WindowLines - opt.WindowOverlap
	// Validate rejects an overlap >= size, which is the only way to get here
	// with a stride that does not advance. Refusing rather than trusting that
	// keeps a caller that skipped Validate from tiling forever.
	if stride <= 0 {
		return nil
	}
	var out []Chunk
	for start := from; start <= to; start += stride {
		end := min(start+opt.WindowLines-1, to)
		out = append(out, Chunk{
			Kind:      models.KindFile,
			StartLine: start,
			EndLine:   end,
			Text:      strings.Join(ls[start-1:end], "\n"),
		})
		// The stride would emit a window ending here again, and a window
		// wholly inside its predecessor is a duplicate document competing
		// with the span it duplicates.
		if end == to {
			break
		}
	}
	return out
}
