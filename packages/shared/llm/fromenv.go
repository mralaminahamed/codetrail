package llm

import (
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/config"
)

// allowedLLMHosts is the compiled-in exact-host allowlist. Not a knob: the
// point of the control is that the destination is chosen by the operator from a
// set this repository decided, so a setting that widened it would be the hole
// it exists to close.
var allowedLLMHosts = []string{"api.anthropic.com"}

const (
	defaultBaseURL = "https://api.anthropic.com"
	// Not a cost choice. An operator who wants a cheaper model sets LLM_MODEL,
	// and the README publishes what one request can cost so that choice has a
	// number in front of it.
	defaultModel = "claude-opus-5"
)

// Providers is the closed set LLM_PROVIDER may name.
var Providers = []string{"none", "fake", "anthropic"}

// FromEnv builds the model from the environment, and returns (nil, nil) when
// there is none.
//
// none is the DEFAULT, because spec:230-232 makes the extractive path the
// default and says a public demo costs nothing per visitor. With none no client
// is constructed, no key is read and nothing is dialled — an unconfigured
// deployment must not need a key and must not fail to boot because a vendor is
// having an incident.
//
// NO BOOT PROBE, unlike embed.FromEnv, and the difference is recorded here so
// it does not read as an oversight. embed probes Ollama at boot so a bad
// EMBED_MODEL names itself rather than surfacing on the first job, and that
// trade is right for a free local call. It is wrong here: a probe is a PAID
// request on every process start, on a component that autoscales, and its
// failure mode is a gateway that will not boot because a vendor is down — while
// the extractive path, which spec:230 says works offline, is unaffected. So the
// provider is validated structurally at boot and exercised on first use, and
// the cost is that a wrong LLM_MODEL surfaces on the first paid request.
func FromEnv(timeout time.Duration) (Model, error) {
	switch provider := config.Get("LLM_PROVIDER", "none"); provider {
	case "none":
		return nil, nil
	case "fake":
		// A provider that is present and always degrades. Its program is empty,
		// so every call answers "program exhausted" and the loop falls back to
		// the extractive answer — which is precisely the path CI has to prove
		// works with no model and no network. A scripted program here would be
		// invented behaviour with no fixture behind it; a test that wants one
		// builds its own NewFake.
		return NewFake(), nil
	case "anthropic":
		raw := config.Get("LLM_BASE_URL", defaultBaseURL)
		// Parsed rather than handed to the constructor as-is: url.Parse accepts
		// "not a url at all" as a relative path, so the scheme and the host are
		// what actually decide whether this is an address. embed/fromenv.go's
		// shape, plus https only and an exact-host match.
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("LLM_BASE_URL=%q is not an address", raw)
		}
		if u.Scheme != "https" {
			return nil, fmt.Errorf("LLM_BASE_URL=%q must use https, got scheme %q", raw, u.Scheme)
		}
		if !slices.Contains(allowedLLMHosts, u.Hostname()) {
			// Refused at boot, not per request. P4's precedent is exact: a
			// setting that turns an outbound connection into one of somebody
			// else's choosing must not be able to start a worker.
			return nil, fmt.Errorf("LLM_BASE_URL=%q names host %q, which is not one of %v",
				raw, u.Hostname(), allowedLLMHosts)
		}
		key, err := LoadSecret()
		if err != nil {
			return nil, err
		}
		return NewAnthropic(raw, config.Get("LLM_MODEL", defaultModel), key, timeout)
	default:
		return nil, fmt.Errorf("LLM_PROVIDER must be one of %v, got %q", Providers, provider)
	}
}
