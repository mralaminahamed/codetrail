package chunk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// astChunks returns one chunk per top-level declaration, or windows over the
// whole file when it is not Go or will not parse.
//
// SkipObjectResolution: nothing here needs the resolver's identifier links,
// and spec §7 is explicit that chunking uses no type information.
func astChunks(filename string, src []byte, ls []string, opt Options) []Chunk {
	if !strings.HasSuffix(filename, ".go") {
		return windows(ls, 1, len(ls), opt)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return windows(ls, 1, len(ls), opt)
	}

	var out []Chunk
	for _, d := range f.Decls {
		kind, symbol, start, end, ok := Decl(fset, d)
		if !ok {
			continue
		}
		if end-start+1 > opt.MaxDeclLines {
			out = append(out, windows(ls, start, end, opt)...)
			continue
		}
		out = append(out, Chunk{
			Kind: kind, Symbol: symbol,
			StartLine: start, EndLine: end,
			Text: strings.Join(ls[start-1:end], "\n"),
		})
	}
	return out
}

// line is the physical line of pos, ignoring //line directives. fset.Position
// applies them, and a generated file that says //line foo.y:3000 would then
// name lines this file does not have — a citation into another file, and a
// slice bound past the end of ls.
func line(fset *token.FileSet, pos token.Pos) int {
	return fset.PositionFor(pos, false).Line
}

// Decl reports what a declaration becomes: its kind, its symbol, and its
// 1-based inclusive line range including any doc comment. Imports are refused:
// an import block is not a retrievable unit, and it is the one declaration
// whose text repeats across thousands of files, so it would be a bank of
// near-duplicate vectors competing with real answers.
//
// Exported and shared with packages/shared/symbols rather than copied: a
// method is Store.Get in spans.symbol, in P3's lexical tsvector through
// replace(symbol,'.',' ') and in every citation already, so a second
// derivation would agree today and drift the day receivers change shape.
// The range comes back for the same reason — a span starts at the doc comment,
// and a definition that started at the func keyword would differ from its own
// span for every documented declaration in the corpus.
func Decl(fset *token.FileSet, d ast.Decl) (models.SpanKind, string, int, int, bool) {
	var kind models.SpanKind
	var symbol string
	var doc *ast.CommentGroup
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Name == nil {
			return "", "", 0, 0, false
		}
		kind, symbol, doc = models.KindFunc, d.Name.Name, d.Doc
		if d.Recv != nil && len(d.Recv.List) > 0 {
			if base := recvBase(d.Recv.List[0].Type); base != "" {
				symbol = base + "." + d.Name.Name
			}
		}
	case *ast.GenDecl:
		switch d.Tok {
		case token.TYPE:
			kind = models.KindType
		case token.CONST:
			kind = models.KindConst
		case token.VAR:
			kind = models.KindVar
		default:
			return "", "", 0, 0, false
		}
		// One span per Decl (spec §5), so a parenthesised block is one span
		// and its symbol is the first name it declares — enough for the
		// lexical arm to find it.
		symbol, doc = firstName(d), d.Doc
	default:
		return "", "", 0, 0, false
	}
	start := line(fset, d.Pos())
	if doc != nil {
		start = line(fset, doc.Pos())
	}
	// End() is one past the declaration's last byte, which for a decl closing
	// with "}" is the newline on the same line — so this is the last line the
	// declaration occupies, inclusive.
	end := line(fset, d.End())
	return kind, symbol, start, end, true
}

func firstName(d *ast.GenDecl) string {
	for _, s := range d.Specs {
		switch s := s.(type) {
		case *ast.TypeSpec:
			if s.Name != nil {
				return s.Name.Name
			}
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				return s.Names[0].Name
			}
		}
	}
	return ""
}

// recvBase unwraps a receiver to the type name a method is searched for by:
// *Counter, Stack[T] and *Stack[K, V] all answer with the bare type.
func recvBase(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvBase(t.X)
	case *ast.IndexExpr:
		return recvBase(t.X)
	case *ast.IndexListExpr:
		return recvBase(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}
