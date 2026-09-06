// Package symbols reads the definitions and call sites of one file out of its
// AST. No database, no type information, no network, no clock.
//
// Nothing here binds a name to a definition. A call records the name it can
// see and where it is; only the type-checker can say which object that name
// means, and a guess made here would produce an edge claiming a precision no
// column records (spec §6, spec:84).
package symbols

import "github.com/mralaminahamed/codetrail/packages/shared/models"

// Def is one top-level declaration.
//
// A definition exists whether or not a span does: chunk sub-windows a
// declaration longer than MaxDeclLines into kind=file spans, so the biggest
// functions in a repository have no span of their own — and they are the ones
// most worth asking who calls them.
//
// StartLine and EndLine are 1-based and inclusive and include the doc comment,
// because they come from the same chunk.Decl a span's range does. They do NOT
// always AGREE with that range, and the difference is not cosmetic: a caller
// that parses the raw body and chunks StripDocs' output — which the indexer
// does, deliberately, and which is how both eval corpora are built — is
// comparing a definition that starts at the doc comment against a span that
// starts at the keyword. Measured: 64.0% of this repository's own definitions
// have a first line no span contains under those two settings. Link by range
// overlap, not by containment of StartLine.
type Def struct {
	Kind      models.SpanKind
	Name      string
	Pkg       string
	Path      string
	StartLine int
	EndLine   int
}

// Call is one call site.
//
// Name is the callee's last identifier — spec:84's "it calls something named
// Close" — because without type information nothing here can tell a package
// qualifier from a variable, and recording s.Get would record a receiver
// expression as though it were a qualified name.
//
// Offset is the byte offset of that identifier and it is the key: a(b(), b())
// is two calls on one line, and the edge id is a hash of the key, so a line
// key merges them into one row.
//
// It is a key into the bytes this call parsed and no others. StripDocs keeps
// every line number but removes the prose bytes, so a caller that resolves
// these keys against a file on disk must hand this pass the same bytes.
type Call struct {
	Path   string
	Name   string
	Offset int
	Line   int
	// FromStart is the first line of the declaration the call is in, which is
	// what names the Def the edge leaves from.
	FromStart int
}

// File is what one source file contributes to the graph.
type File struct {
	Pkg   string
	Defs  []Def
	Calls []Call
	// Unnameable counts callees that are neither an identifier nor a
	// selector: f()(), fns[i](), func(){}(). edges.to_name is NOT NULL, and
	// an edge naming nothing is not a weaker claim than a syntactic one, it
	// is an empty one.
	Unnameable int
}
