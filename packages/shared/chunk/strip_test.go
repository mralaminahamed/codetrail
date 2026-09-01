package chunk

import (
	"strings"
	"testing"
)

func stripped(t *testing.T, name string) ([]byte, []byte, []string) {
	t.Helper()
	src, ls := fixture(t, name)
	out, err := StripDocs("docs.go", src)
	if err != nil {
		t.Fatal(err)
	}
	return src, out, ls
}

// The invariant the whole design rests on: line numbers do not move, so a
// window over the stripped source covers the same lines as one over the
// original and the two arms stay comparable.
//
// The fixture's doc comments include a seven-line /* */ block, which is the
// only place a stripper can swallow a newline — a comment's Pos..End stops
// before the newline that ends a // line, so a fixture of line comments alone
// cannot tell blanking from dropping newlines and would prove nothing.
func TestStripDocsPreservesEveryLineNumber(t *testing.T) {
	_, out, ls := stripped(t, "docs.gotxt")
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(ls) {
		t.Fatalf("stripped source has %d lines, original has %d", len(got), len(ls))
	}
	// Blanking empties a line or leaves its indentation; it never rewrites one.
	for i := range got {
		if strings.TrimSpace(got[i]) != "" && got[i] != ls[i] {
			t.Fatalf("line %d changed: %q -> %q", i+1, ls[i], got[i])
		}
	}
	// Pinned by number, because "unchanged content" alone would still hold if
	// every line slid up together. Line 25 is the one directly below the block
	// comment, so it is the first to move if its newlines go.
	for _, tc := range []struct {
		line int
		want string
	}{
		{1, "//go:build linux"},
		{7, "package docs"},
		{16, `var ErrGone = errors.New("gone")`},
		{25, "const ("},
		{41, "func (c *Counter) Add(d int) int {"},
		{48, `func undocumented() error { return fmt.Errorf("no doc") }`},
	} {
		if got[tc.line-1] != tc.want {
			t.Fatalf("line %d holds %q, want %q", tc.line, got[tc.line-1], tc.want)
		}
	}
}

// Prose out, code in. The question is the doc comment; the haystack must be
// code bodies only, or the question is a substring of its own answer.
func TestStripDocsRemovesDocProseAndKeepsCode(t *testing.T) {
	_, out, _ := stripped(t, "docs.gotxt")
	for _, gone := range []string{
		"Package docs is the fixture", // file doc
		"Errors is the standard one",  // import spec doc
		"ErrGone is returned",         // var decl doc
		"Kinds are the shapes",        // block comment on a const decl
		"KindA is the first shape",    // value spec doc inside the block
		"Counter counts",              // type decl doc
		"N is the running total",      // struct field doc
		"Add adds d and returns",      // method doc
		"spans more than one line",    // its second paragraph
	} {
		if strings.Contains(string(out), gone) {
			t.Fatalf("doc prose %q survived stripping", gone)
		}
	}
	for _, kept := range []string{
		"package docs",
		`var ErrGone = errors.New("gone")`,
		`KindA = "a"`,
		"N int",
		"func (c *Counter) Add(d int) int {",
		"c.N += d",
		`func undocumented() error { return fmt.Errorf("no doc") }`,
	} {
		if !strings.Contains(string(out), kept) {
			t.Fatalf("code %q was removed", kept)
		}
	}
}

// The property the pre-pass exists to buy: the baseline arm's windows land on
// exactly the same lines over the stripped corpus as over the original, so the
// two arms differ in where the cuts fall and in nothing else.
func TestStrippedSourceWindowsToTheSameRanges(t *testing.T) {
	src, out, _ := stripped(t, "docs.gotxt")
	before, _ := Chunks("docs.go", src, winOpts(10, 2))
	after, err := Chunks("docs.go", out, winOpts(10, 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("stripping changed the window count: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].StartLine != after[i].StartLine || before[i].EndLine != after[i].EndLine {
			t.Fatalf("window %d moved from %d..%d to %d..%d", i,
				before[i].StartLine, before[i].EndLine, after[i].StartLine, after[i].EndLine)
		}
	}
}

// The AST arm chunks the stripped source, so it has to still parse and still
// find the same declarations ending on the same lines.
//
// The start line does move: it was the doc comment's, and a blanked comment is
// no longer a comment, so the parser starts the declaration at its keyword.
// That is the one thing blanking does not preserve, and it is harmless — the
// span still covers all of the code it did. What must not move is the end,
// because everything below a deleted comment would shift.
func TestStrippedSourceStillChunksToTheSameDeclarations(t *testing.T) {
	src, out, _ := stripped(t, "docs.gotxt")
	sl := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	before, _ := Chunks("docs.go", src, astOpts())
	after, err := Chunks("docs.go", out, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("stripping changed the declaration count: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Symbol != after[i].Symbol || before[i].Kind != after[i].Kind {
			t.Fatalf("chunk %d: %s/%s -> %s/%s", i,
				before[i].Kind, before[i].Symbol, after[i].Kind, after[i].Symbol)
		}
		if before[i].EndLine != after[i].EndLine {
			t.Fatalf("%s ends at %d, was %d", after[i].Symbol, after[i].EndLine, before[i].EndLine)
		}
		if after[i].StartLine < before[i].StartLine {
			t.Fatalf("%s starts at %d, above its original %d", after[i].Symbol,
				after[i].StartLine, before[i].StartLine)
		}
		if strings.TrimSpace(sl[after[i].StartLine-1]) == "" {
			t.Fatalf("%s starts at line %d, which is blank", after[i].Symbol, after[i].StartLine)
		}
	}
	// Named, because the loop above would pass on a file with no doc comments
	// at all: this is the span the golden set's question is generated from.
	add := byName(t, after, "Counter.Add")
	if strings.Contains(add.Text, "Add adds d and returns") {
		t.Fatalf("Counter.Add still holds its doc prose:\n%s", add.Text)
	}
	if !strings.Contains(add.Text, "c.N += d") {
		t.Fatalf("Counter.Add lost its body:\n%s", add.Text)
	}
	if !strings.Contains(byName(t, before, "Counter.Add").Text, "Add adds d and returns") {
		t.Fatal("the unstripped fixture has no doc prose on Counter.Add, so this test proves nothing")
	}
}

func byName(t *testing.T, cs []Chunk, symbol string) Chunk {
	t.Helper()
	for _, c := range cs {
		if c.Symbol == symbol {
			return c
		}
	}
	t.Fatalf("no chunk for %s", symbol)
	return Chunk{}
}

// Only doc comments. A comment inside a function body is code context a reader
// of the span wants, and no golden question is generated from it.
func TestStripDocsKeepsCommentsInsideBodies(t *testing.T) {
	_, out, _ := stripped(t, "docs.gotxt")
	if !strings.Contains(string(out), "// This comment is inside the body and stays") {
		t.Fatalf("body comment was removed:\n%s", out)
	}
	src := []byte("package p\n\n// Doc prose.\nfunc F() {\n\t// inside the body\n\t_ = 1\n}\n")
	got, err := StripDocs("f.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "Doc prose") {
		t.Fatal("doc comment survived")
	}
	if !strings.Contains(string(got), "// inside the body") {
		t.Fatalf("body comment was removed:\n%s", got)
	}
}

// The package doc is prose too, and it is the largest single block of it in a
// well-documented repository. Nothing else in the walk reaches it: ast.Inspect
// visits a File's Doc as a CommentGroup, which the node switch does not match.
func TestStripDocsRemovesThePackageDoc(t *testing.T) {
	src := []byte("// Package p does a thing.\n//\n// At length.\npackage p\n\nfunc F() {}\n")
	out, err := StripDocs("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "does a thing") || strings.Contains(string(out), "At length") {
		t.Fatalf("package doc survived:\n%s", out)
	}
	if !strings.Contains(string(out), "package p") {
		t.Fatalf("package clause was removed:\n%s", out)
	}
}

// A build constraint is not prose, and removing it changes what the file means.
// //go:generate and //nolint are the same case.
//
// The directive that decides this is //go:noinline, which sits inside a doc
// comment group. A leading //go:build is separated from the package clause by a
// blank line, so it is a floating comment no Doc points at and it survives
// whatever this function does — it cannot tell a working skip from a missing one.
func TestStripDocsKeepsDirectives(t *testing.T) {
	_, out, _ := stripped(t, "docs.gotxt")
	for _, kept := range []string{"//go:build linux", "//go:noinline"} {
		if !strings.Contains(string(out), kept) {
			t.Fatalf("directive %q was stripped:\n%s", kept, out)
		}
	}
	if strings.Contains(string(out), "The second paragraph exists") {
		t.Fatalf("doc prose survived alongside the directive:\n%s", out)
	}
	// Prose that only looks like a directive is prose: the name before the
	// colon has to be lowercase alphanumeric.
	src := []byte("package p\n\n// TODO: explain this.\n//go:generate stringer -type=K\nfunc F() {}\n")
	got, err := StripDocs("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "TODO: explain") {
		t.Fatalf("a comment with a colon in it was kept as a directive:\n%s", got)
	}
	if !strings.Contains(string(got), "//go:generate stringer -type=K") {
		t.Fatalf("//go:generate was stripped:\n%s", got)
	}
}

// A file that will not parse has no Go doc comments to find. Returning an error
// rather than the source unchanged is the conservative call: the alternative is
// a benchmark corpus that silently holds the prose its questions were generated
// from, and nothing about the run would look wrong.
func TestStripDocsRefusesWhatItCannotParse(t *testing.T) {
	src, _ := fixture(t, "broken.gotxt")
	if _, err := StripDocs("broken.go", src); err == nil {
		t.Fatal("unparseable Go was stripped without error")
	}
}

// Not Go: nothing to strip, and no error. Markdown in the corpus is windowed as
// prose and carries no doc comment a golden question comes from.
func TestStripDocsPassesNonGoThrough(t *testing.T) {
	md := []byte("# Title\n\n// not a doc comment\n")
	out, err := StripDocs("README.md", md)
	if err != nil || string(out) != string(md) {
		t.Fatalf("markdown was altered: %q, %v", out, err)
	}
	// Markdown alone does not test the extension check — it fails to parse, so
	// dropping the check turns this into an error rather than a changed file.
	// What the check decides is a non-Go file whose bytes are valid Go: a
	// .gotxt fixture, a vendored .go.orig. Those must come back untouched.
	src, _ := fixture(t, "docs.gotxt")
	out, err = StripDocs("docs.gotxt", src)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(src) {
		t.Fatal("valid Go under a non-.go name was stripped")
	}
}

// //line directives retarget go/token's positions at the generator's source,
// including past the end of this file, and Task 2 found that hazard live: the
// AST chunker needs PositionFor(pos, false) or a span lands in another file.
//
// It does not reach here. Measured: swapping tf.Offset for fset.Position's
// Offset leaves every test in this file passing, because a directive moves the
// line a position reports and never the byte it sits at. What this test does
// pin is that a directive numbered past the end of the file is prose-free and
// survives, rather than being blanked or panicking on a bad offset.
func TestStripDocsSurvivesLineDirectives(t *testing.T) {
	src := []byte("package p\n\n//line sample.y:3000\n// F does a thing.\nfunc F() {\n\t_ = 1\n}\n")
	out, err := StripDocs("gen.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "F does a thing") {
		t.Fatalf("doc prose survived:\n%s", out)
	}
	if !strings.Contains(string(out), "//line sample.y:3000") {
		t.Fatalf("//line directive was stripped:\n%s", out)
	}
	if n := strings.Count(string(out), "\n"); n != strings.Count(string(src), "\n") {
		t.Fatalf("stripped source has %d newlines, original has %d", n, strings.Count(string(src), "\n"))
	}
}
