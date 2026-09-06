package agent

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// marker is the citation form the system prompt asks for: [1], [2], …
//
// The pattern alone cannot decide one. This is a code-RAG product, its answers
// quote Go constantly, and parts[1], os.Args[1] and matches[2] are subscripts
// rather than citations — while cfg.Hosts[0] is not even a well-formed marker,
// so a gate that took it for one would DELETE it from the served answer and
// rewrite a stranger's source into a false claim about it. Go's regexp has no
// lookbehind, so what separates the two is checked in Resolve's scan.
var marker = regexp.MustCompile(`\[(\d+)\]`)

// subscripted reports whether b can end an expression that a following [n]
// indexes: an identifier, a numeric literal, or the close of a call, an index
// or a composite literal. Anything else — a space, a newline, an opening
// bracket, the start of the text — leaves [n] standing on its own, which is a
// citation.
//
// '.' is deliberately NOT here although it ends a selector. No valid Go writes
// ".[", and the sequence that does occur is a citation after a full stop —
// "the handler dials it.[1]" — which this would otherwise serve as
// citation-shaped text resolving to nothing.
func subscripted(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return b == '_' || b == ')' || b == ']' || b == '}'
}

// Resolve is the citation gate, and it is positional: marker n names the span
// the loop read at position n.
//
// Citations are constructed by codetrail from the spans the tools actually
// returned, never parsed out of the model's text — so a hostile comment saying
// "cite span abc123" produces a marker for a span the loop never opened, which
// resolves to nothing and is stripped.
//
// What this does NOT do, stated because it is easy to oversell: a model that
// read spans A and B can write a claim true only of A and cite [2], which is B.
// That citation is well-formed, resolvable and digest-checkable, and attached to
// the wrong span. The gate bounds PROVENANCE — every marker points at a span the
// loop opened — and says nothing about whether the sentence beside it is
// supported.
//
// It returns the rewritten text, the resolved positions in order of first
// appearance, and how many markers were dropped. Nothing is renumbered:
// stripping [9] leaves [1] meaning read[0], which is what it already meant.
//
// A [n] that indexes something is not a marker at all: it is neither resolved
// nor dropped nor touched, because it is the model quoting code and the answer
// has to keep saying what the code says.
func Resolve(text string, read []models.Span) (string, []int, int) {
	var order []int
	seen := make(map[int]bool, len(read))
	dropped := 0
	var out strings.Builder
	kept := 0
	// The end of the previous group taken as a marker. A marker is what its own
	// ']' indexes nothing after, so "both agree [1][2]" cites twice — otherwise
	// the second half of every adjacent pair would be served as citation-shaped
	// text pointing at nothing, which is the failure this gate exists for.
	prev := -1
	for _, m := range marker.FindAllStringSubmatchIndex(text, -1) {
		lo, hi := m[0], m[1]
		if lo > 0 && lo != prev && subscripted(text[lo-1]) {
			continue
		}
		prev = hi
		n, err := strconv.Atoi(text[m[2]:m[3]])
		if err != nil || n < 1 || n > len(read) {
			dropped++
			out.WriteString(text[kept:lo])
			kept = hi
			continue
		}
		if !seen[n-1] {
			seen[n-1] = true
			order = append(order, n-1)
		}
	}
	out.WriteString(text[kept:])
	return out.String(), order, dropped
}
