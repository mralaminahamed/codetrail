package llm

import (
	"errors"
	"strconv"
)

// Kind is why a provider did not answer. Five values, closed, because each one
// drives a different degradation branch and a different counter label: spec:234
// names a rate limit and an expired key as two distinct events, and collapsing
// them makes "somebody has to rotate a key" indistinguishable from "wait a
// minute".
type Kind string

const (
	KindRateLimited  Kind = "rate_limited"
	KindUnauthorized Kind = "unauthorized"
	KindUnavailable  Kind = "provider_unavailable"
	KindDeadline     Kind = "deadline"
	// KindMalformed is a provider that answered something that is not a
	// Response — a truncated body, a tool_use block with no name. It is NOT a
	// tool call whose arguments are wrong: that is the loop's business, and one
	// label over both would make a broken provider and a confused model the
	// same series.
	KindMalformed Kind = "malformed_response"
)

// Kinds is the vocabulary, exported so the metric labels and the degradation
// table read one list. A sixth kind added without a label value is a series
// that never initialises to zero, and rate() over an absent series is nothing
// at all (metrics.go:106-109).
var Kinds = []Kind{KindRateLimited, KindUnauthorized, KindUnavailable, KindDeadline, KindMalformed}

func (k Kind) String() string { return string(k) }

// maxDetail bounds what a provider's error body may contribute to an error
// string. embed/ollama.go:71-79 bounds its own to 512 for a different reason;
// here the reason is worse — a provider's error body can echo the request, and
// the request carries a stranger's repository text.
const maxDetail = 512

// Failure is a provider that refused, as a struct rather than five sentinels,
// because Status and Detail are needed for one log line and must never reach a
// response body.
type Failure struct {
	Kind   Kind
	Status int
	Detail string
}

func (f *Failure) Error() string {
	s := "llm: " + string(f.Kind)
	if f.Status != 0 {
		s += " (http " + strconv.Itoa(f.Status) + ")"
	}
	if f.Detail != "" {
		d := f.Detail
		if len(d) > maxDetail {
			d = d[:maxDetail]
		}
		s += ": " + d
	}
	return s
}

// Classify answers (kind, true) for a provider failure and (_, false) for
// anything else.
//
// Two returns rather than one, because "not a provider failure" has to be a
// distinct answer from "unavailable": a nil dereference inside the loop
// reported as provider_unavailable is a bug filed as an outage, and with a
// single return there is no value that could say so.
func Classify(err error) (Kind, bool) {
	var f *Failure
	if errors.As(err, &f) {
		return f.Kind, true
	}
	return "", false
}
