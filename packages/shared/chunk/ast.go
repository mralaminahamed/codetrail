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
		kind, symbol, ok := classify(d)
		if !ok {
			continue
		}
		start := line(fset, d.Pos())
		if doc := docOf(d); doc != nil {
			start = line(fset, doc.Pos())
		}
		// End() is one past the declaration's last byte, which for a decl
		// closing with "}" is the newline on the same line — so this is the
		// last line the declaration occupies, inclusive.
		end := line(fset, d.End())
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

// classify names the span a declaration becomes, or refuses it. Imports are
// refused: an import block is not a retrievable unit, and it is the one
// declaration whose text repeats across thousands of files, so it would be a
// bank of near-duplicate vectors competing with real answers.
func classify(d ast.Decl) (models.SpanKind, string, bool) {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Name == nil {
			return "", "", false
		}
		if d.Recv != nil && len(d.Recv.List) > 0 {
			if base := recvBase(d.Recv.List[0].Type); base != "" {
				return models.KindFunc, base + "." + d.Name.Name, true
			}
		}
		return models.KindFunc, d.Name.Name, true
	case *ast.GenDecl:
		var kind models.SpanKind
		switch d.Tok {
		case token.TYPE:
			kind = models.KindType
		case token.CONST:
			kind = models.KindConst
		case token.VAR:
			kind = models.KindVar
		default:
			return "", "", false
		}
		// One span per Decl (spec §5), so a parenthesised block is one span
		// and its symbol is the first name it declares — enough for the
		// lexical arm to find it.
		return kind, firstName(d), true
	}
	return "", "", false
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

func docOf(d ast.Decl) *ast.CommentGroup {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return d.Doc
	case *ast.GenDecl:
		return d.Doc
	}
	return nil
}
