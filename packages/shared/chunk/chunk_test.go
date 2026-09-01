package chunk

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func lines(n int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString("line")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func winOpts(size, overlap int) Options {
	o := Defaults()
	o.Strategy = StrategyWindow
	o.WindowLines, o.WindowOverlap = size, overlap
	return o
}

// Fail closed, the way walk.Limits and clone.Limits do: a zero from
// config.GetInt is a misconfiguration, not permission to do something
// unbounded. An overlap >= size is an infinite loop, so it is refused here
// rather than discovered in production.
func TestInvalidOptionsAreRefused(t *testing.T) {
	for _, o := range []Options{
		{Strategy: StrategyWindow, WindowLines: 0, WindowOverlap: 0, MaxDeclLines: 100},
		{Strategy: StrategyWindow, WindowLines: 5, WindowOverlap: 5, MaxDeclLines: 100},
		{Strategy: StrategyWindow, WindowLines: 5, WindowOverlap: 6, MaxDeclLines: 100},
		{Strategy: StrategyWindow, WindowLines: 5, WindowOverlap: -1, MaxDeclLines: 100},
		{Strategy: StrategyWindow, WindowLines: 5, WindowOverlap: 1, MaxDeclLines: 0},
		{Strategy: "banana", WindowLines: 5, WindowOverlap: 1, MaxDeclLines: 100},
	} {
		if _, _, err := Chunks("a.txt", lines(10), o); err == nil {
			t.Fatalf("%+v was accepted", o)
		}
	}
}

// An empty file has nothing to retrieve. One empty span per empty file would
// be an embedding call and a row for zero bytes of content.
func TestEmptyFileYieldsNoChunks(t *testing.T) {
	for _, src := range [][]byte{nil, {}, []byte("\n"), []byte("   \n\t\n")} {
		got, _, err := Chunks("a.txt", src, winOpts(5, 1))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("%q produced %d chunks", src, len(got))
		}
	}
}

// blanked builds n lines where those in [from,to] hold only whitespace, which
// is what StripDocs leaves behind where a doc comment was.
func blanked(n, from, to int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i >= from && i <= to {
			b.WriteString("   \n")
			continue
		}
		b.WriteString("line")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func ranges(cs []Chunk) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, strconv.Itoa(c.StartLine)+".."+strconv.Itoa(c.EndLine))
	}
	return out
}

// A window of blank lines is not a span: there is nothing in it to retrieve,
// and no vector for it that is safe to store.
//
// Measured on the eval's own configuration — CHUNK_STRATEGY=window,
// STRIP_DOC_COMMENTS=true over rs/zerolog on the fake embedder — before this
// dropped: the whole job failed with "embedding spans 576-607: embed: text 19
// has no tokens to hash", 0 repos and 0 spans written, three times to terminal.
func TestAWindowWithNothingToEmbedIsDropped(t *testing.T) {
	got, dropped, err := Chunks("a.txt", blanked(15, 6, 10), winOpts(5, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1..5", "11..15"}; !slices.Equal(ranges(got), want) {
		t.Fatalf("windows are %v, want %v — 6..10 holds only blank lines", ranges(got), want)
	}
	// Counted, not silent: a corpus that quietly loses spans is how it shrinks
	// without anyone noticing, and the job still succeeds.
	if dropped != 1 {
		t.Fatalf("dropped %d, want 1", dropped)
	}
}

// The same rule under the AST strategy, over a declaration long enough to be
// sub-windowed — the one place that arm emits a window of its own. An arm that
// kept a region the other dropped would bias the comparison spec §9 exists to
// make, in the one property it is measuring.
func TestTheASTArmDropsABlankSubWindowToo(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n\nfunc Big() {\n\t_ = 1\n")
	for range 10 { // lines 5..14
		b.WriteString("   \n")
	}
	b.WriteString("\t_ = 2\n}\n")
	o := astOpts()
	o.MaxDeclLines, o.WindowLines, o.WindowOverlap = 5, 5, 0

	got, dropped, err := Chunks("big.go", []byte(b.String()), o)
	if err != nil {
		t.Fatal(err)
	}
	// The declaration is lines 3..16, so it tiles 3..7, 8..12, 13..16 and the
	// middle one is nothing but the blank lines.
	if want := []string{"3..7", "13..16"}; !slices.Equal(ranges(got), want) {
		t.Fatalf("sub-windows are %v, want %v", ranges(got), want)
	}
	if dropped != 1 {
		t.Fatalf("dropped %d, want 1", dropped)
	}
}
