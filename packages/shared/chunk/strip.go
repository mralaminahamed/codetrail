package chunk

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// StripDocs blanks every doc comment in src, keeping its newlines so no line
// number moves.
//
// It exists for the eval (spec §9). The golden set's questions are doc-comment
// prose and its answers are the spans those comments document, so a corpus that
// keeps the comments makes every question a literal substring of its own answer
// and both arms score on string overlap alone. The production index keeps them,
// because there they are the best signal available.
//
// A pre-pass rather than a chunker mode: applied before Chunks, both eval arms
// chunk byte-identical input by construction, and StrategyWindow still never
// parses (spec §5, which asks for both and cannot have them any other way).
//
// Blanked, not deleted. Measured on testdata/docs.gotxt: deleting the lines
// instead moves Counter.Add from 37..46 to 22..28 and turns six windows into
// four, so the two arms would tile different lines and every citation into the
// stripped corpus would name code that is not there.
//
// It is the *lines* that survive, not the bytes: the prose is removed and only
// its line terminators are kept, so line numbers are invariant and byte
// offsets are not. The symbols package measures it, because a caller keying
// anything by offset must be given the same bytes it will later resolve
// against.
func StripDocs(filename string, src []byte) ([]byte, error) {
	if !strings.HasSuffix(filename, ".go") {
		return src, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		// Refused rather than passed through: the alternative is an eval corpus
		// that silently holds the prose its questions came from, and nothing
		// about the run would look wrong.
		return nil, fmt.Errorf("chunk: strip %s: %w", filename, err)
	}
	tf := fset.File(f.Pos())

	// drop[i] is true for a byte of doc prose. A mask rather than a list of
	// ranges so the traversal below can visit them in any order.
	drop := make([]bool, len(src))
	add := func(g *ast.CommentGroup) {
		if g == nil {
			return
		}
		for _, c := range g.List {
			if isDirective(c.Text) {
				continue
			}
			start := tf.Offset(c.Pos())
			for i := start; i < commentEnd(src, start); i++ {
				drop[i] = true
			}
		}
	}
	// f.Doc is not reached by Inspect's switch below, and it is the largest
	// single block of prose in a well-documented file.
	add(f.Doc)
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			add(d.Doc)
		case *ast.GenDecl:
			add(d.Doc)
		case *ast.TypeSpec:
			add(d.Doc)
		case *ast.ValueSpec:
			add(d.Doc)
		case *ast.ImportSpec:
			add(d.Doc)
		case *ast.Field:
			add(d.Doc)
		}
		return true
	})

	out := make([]byte, 0, len(src))
	for i, b := range src {
		// The line terminator is what is kept: it is the whole of "blanked,
		// not deleted", and only a /* */ doc comment has any inside it. Under
		// CRLF the terminator is both bytes, so keeping only the \n would
		// rewrite every line ending inside a block doc comment.
		if drop[i] && b != '\n' && b != '\r' {
			continue
		}
		out = append(out, b)
	}
	if err := selfCheck(filename, src, out); err != nil {
		return nil, err
	}
	return out, nil
}

// commentEnd is the offset one past the comment starting at start, read from
// the source rather than from ast.Comment.End().
//
// End() is Slash+len(c.Text), and go/scanner strips carriage returns out of
// c.Text: on a CRLF file a /* */ doc comment's Text is shorter than the bytes
// it occupies, End() lands inside it, and the tail — a bare "*/" — is left
// behind in source that then no longer parses.
func commentEnd(src []byte, start int) int {
	if start+1 < len(src) && src[start+1] == '*' {
		if i := bytes.Index(src[start+2:], []byte("*/")); i >= 0 {
			return start + 2 + i + 2
		}
		return len(src) // unterminated: unreachable, the parse already failed
	}
	// A // comment ends before its newline, which is not part of it.
	if i := bytes.IndexByte(src[start:], '\n'); i >= 0 {
		return start + i
	}
	return len(src)
}

// selfCheck refuses output that broke either invariant the callers rely on:
// that the result is still the same Go file minus prose, and that no line
// number moved. Both are cheap next to the parse this function already does,
// and the CRLF bug above is what they exist for — it corrupted the corpus for
// free, because the AST arm's answer to source that will not parse is windows
// rather than an error.
var lf = []byte("\n")

func selfCheck(filename string, src, out []byte) error {
	if got, want := bytes.Count(out, lf), bytes.Count(src, lf); got != want {
		return fmt.Errorf("chunk: strip %s: %d lines out, %d in", filename, got, want)
	}
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, filename, out, parser.ParseComments|parser.SkipObjectResolution); err != nil {
		return fmt.Errorf("chunk: strip %s: stripped source no longer parses: %w", filename, err)
	}
	return nil
}

// isDirective reports whether a comment is a directive rather than prose.
// Blanking one changes what the file means: //go:build decides whether it
// compiles at all, and //line decides what the rest of it claims to be.
//
// Hand-rolled because go/ast keeps its own copy unexported: at Go 1.27.0,
// `go doc go/ast.IsDirective` answers "no symbol IsDirective in package go/ast".
// The rule is go/ast's — the three grandfathered words, then //name:value with
// name lowercase alphanumeric, which is what excludes prose like "// TODO: x".
//
// The //-prefix check is a guard rather than a decision: measured, deleting it
// changes no answer, because a /* */ comment's text starts with "/" and the
// name loop refuses it anyway.
func isDirective(text string) bool {
	c, ok := strings.CutPrefix(text, "//")
	if !ok {
		return false
	}
	for _, word := range []string{"line ", "extern ", "export "} {
		if strings.HasPrefix(c, word) {
			return true
		}
	}
	colon := strings.Index(c, ":")
	if colon <= 0 || colon+1 >= len(c) {
		return false
	}
	for i := 0; i < colon; i++ {
		if !isLowerAlnum(c[i]) {
			return false
		}
	}
	return isLowerAlnum(c[colon+1])
}

func isLowerAlnum(b byte) bool {
	return 'a' <= b && b <= 'z' || '0' <= b && b <= '9'
}
