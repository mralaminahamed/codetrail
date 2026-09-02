package rag

import (
	"strings"
	"unicode"
)

// maxTerms bounds the tsquery a question can build. The edge already refuses a
// 1,000-character question; this bounds what the ones that get through can ask
// Postgres to OR together.
const maxTerms = 32

// Terms turns a question into the terms of an OR-ed tsquery: lowercased runs of
// letters and digits, plus — when split is on — each run's camel-case parts.
//
// The charset is what makes the result safe to hand to to_tsquery. &, |, !, :
// and ( are operators there, and most questions about code contain one, so the
// raw string is a syntax error rather than a query.
//
// Written against what pg17's simple configuration actually does, measured
// rather than assumed:
//
//	parseConfig  -> 'parseconfig'      camel case is not split
//	parse_config -> 'parse' 'config'   the underscore is a separator
//	HTTPServer   -> 'httpserver'
//	v2           -> 'v2'
//	x.y          -> 'x.y'              the dot binds rather than separates
//
// So the whole run is kept because it is the token the index holds for a
// camel-case identifier, and splitting on the underscore is not a choice — the
// index already holds those halves apart. The parts are a widener and nothing
// more: the index has no 'parse' inside 'parseconfig', so the split does not
// make "parse config" find parseConfig, it makes parseConfig also match prose
// that spells the parts separately. Closing that asymmetry means splitting at
// index time, which costs a re-index.
//
// The dot is the one place the terms cannot follow the index: a query naming
// x.y or s.pool.Query reaches those spans only through its parts. Method
// symbols are the case that matters and migration 0008 handles them from the
// index side instead.
func Terms(q string, split bool) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		if s == "" || seen[s] || len(out) >= maxTerms {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, run := range wordRuns(q) {
		add(strings.ToLower(run))
		if split {
			for _, part := range camelParts(run) {
				add(strings.ToLower(part))
			}
		}
	}
	return out
}

// wordRuns is the tokeniser proper: maximal runs of letters and digits, which
// is the charset that cannot carry a tsquery operator.
func wordRuns(q string) []string {
	var out []string
	start := -1
	for i, r := range q {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, q[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, q[start:])
	}
	return out
}

// camelParts splits at a lower-to-upper boundary, and inside a run of capitals
// at the one before a lowercase letter — so HTTPServer is HTTP and Server. A
// digit never starts a part: v2 is one token in the index and two terms would
// be two tokens it does not hold.
func camelParts(w string) []string {
	rs := []rune(w)
	var out []string
	start := 0
	for i := 1; i < len(rs); i++ {
		switch {
		case unicode.IsUpper(rs[i]) && !unicode.IsUpper(rs[i-1]):
		case unicode.IsUpper(rs[i]) && unicode.IsUpper(rs[i-1]) &&
			i+1 < len(rs) && unicode.IsLower(rs[i+1]):
		default:
			continue
		}
		out = append(out, string(rs[start:i]))
		start = i
	}
	return append(out, string(rs[start:]))
}
