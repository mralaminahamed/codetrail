package chunk

import (
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// Every window is 1-based inclusive and covers exactly the lines it names.
// Anchored on content, not on arithmetic: a uniform off-by-one moves the text
// with the range and would survive a text-equals-slice check.
func TestWindowRangesAreOneBasedInclusive(t *testing.T) {
	src := lines(10)
	got, err := Chunks("a.txt", src, winOpts(4, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].StartLine != 1 {
		t.Fatalf("first window starts at %d, want 1", got[0].StartLine)
	}
	if first := strings.SplitN(got[0].Text, "\n", 2)[0]; first != "line1" {
		t.Fatalf("line %d holds %q, want line1", got[0].StartLine, first)
	}
	last := got[len(got)-1]
	if last.EndLine != 10 {
		t.Fatalf("last window ends at %d, want 10", last.EndLine)
	}
	tail := strings.Split(strings.TrimRight(last.Text, "\n"), "\n")
	if tail[len(tail)-1] != "line10" {
		t.Fatalf("line %d holds %q, want line10", last.EndLine, tail[len(tail)-1])
	}
	for _, c := range got {
		if c.EndLine-c.StartLine+1 != len(strings.Split(strings.TrimRight(c.Text, "\n"), "\n")) {
			t.Fatalf("%d..%d does not hold that many lines: %q", c.StartLine, c.EndLine, c.Text)
		}
	}
}

// Overlap is what stops a declaration cut in half from being unretrievable in
// both halves. Two adjacent windows must share exactly WindowOverlap lines.
func TestWindowsOverlapByExactlyTheConfiguredLines(t *testing.T) {
	got, err := Chunks("a.txt", lines(20), winOpts(5, 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 {
		t.Fatalf("want several windows, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		shared := got[i-1].EndLine - got[i].StartLine + 1
		if shared != 2 {
			t.Fatalf("windows %d and %d share %d lines, want 2", i-1, i, shared)
		}
	}
}

// The whole file is covered: the baseline arm's claim is that it indexes
// everything, so a gap between windows would be a silently unindexed region.
func TestWindowsCoverEveryLine(t *testing.T) {
	for _, n := range []int{1, 4, 5, 6, 19, 20, 21} {
		got, err := Chunks("a.txt", lines(n), winOpts(5, 2))
		if err != nil {
			t.Fatal(err)
		}
		covered := map[int]bool{}
		for _, c := range got {
			for l := c.StartLine; l <= c.EndLine; l++ {
				covered[l] = true
			}
		}
		for l := 1; l <= n; l++ {
			if !covered[l] {
				t.Fatalf("%d lines: line %d is in no window", n, l)
			}
		}
		if covered[n+1] {
			t.Fatalf("%d lines: a window runs past the end", n)
		}
	}
}

// A trailing window wholly inside its predecessor is a duplicate document with
// a different id: it costs an embedding and can outrank the span it duplicates.
func TestNoWindowIsContainedInAnother(t *testing.T) {
	for _, n := range []int{6, 7, 8, 11, 12} {
		got, err := Chunks("a.txt", lines(n), winOpts(5, 3))
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(got); i++ {
			if got[i].EndLine <= got[i-1].EndLine {
				t.Fatalf("%d lines: window %d (%d..%d) adds nothing over %d..%d",
					n, i, got[i].StartLine, got[i].EndLine, got[i-1].StartLine, got[i-1].EndLine)
			}
		}
	}
}

// Every window is kind=file. The eval tells the two corpora apart by what the
// rows say, and a window claiming to be a declaration would be a lie in the
// only column that could catch it.
func TestWindowsAreKindFile(t *testing.T) {
	got, _ := Chunks("a.go", lines(9), winOpts(4, 1))
	for _, c := range got {
		if c.Kind != models.KindFile || c.Symbol != "" {
			t.Fatalf("got kind=%q symbol=%q, want file/empty", c.Kind, c.Symbol)
		}
	}
}

// The window strategy never parses. Given Go that cannot parse, it still
// windows it — this is the property the baseline arm is built on.
func TestWindowStrategyDoesNotParse(t *testing.T) {
	got, err := Chunks("broken.go", []byte("package p\nfunc ((( {\n"), winOpts(2, 0))
	if err != nil {
		t.Fatalf("window strategy must not fail on unparseable Go: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("want windows over unparseable Go, got none")
	}
}
