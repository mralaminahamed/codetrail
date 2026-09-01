package chunk

import (
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
		if _, err := Chunks("a.txt", lines(10), o); err == nil {
			t.Fatalf("%+v was accepted", o)
		}
	}
}

// An empty file has nothing to retrieve. One empty span per empty file would
// be an embedding call and a row for zero bytes of content.
func TestEmptyFileYieldsNoChunks(t *testing.T) {
	for _, src := range [][]byte{nil, {}, []byte("\n"), []byte("   \n\t\n")} {
		got, err := Chunks("a.txt", src, winOpts(5, 1))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("%q produced %d chunks", src, len(got))
		}
	}
}
