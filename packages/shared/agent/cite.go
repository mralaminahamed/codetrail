package agent

import (
	"regexp"
	"strconv"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// marker is the citation form the system prompt asks for: [1], [2], …
var marker = regexp.MustCompile(`\[(\d+)\]`)

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
func Resolve(text string, read []models.Span) (string, []int, int) {
	var order []int
	seen := make(map[int]bool, len(read))
	dropped := 0
	out := marker.ReplaceAllStringFunc(text, func(m string) string {
		n, err := strconv.Atoi(m[1 : len(m)-1])
		if err != nil || n < 1 || n > len(read) {
			dropped++
			return ""
		}
		if !seen[n-1] {
			seen[n-1] = true
			order = append(order, n-1)
		}
		return m
	})
	return out, order, dropped
}
