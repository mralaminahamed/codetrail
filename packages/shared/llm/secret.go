package llm

import (
	"fmt"
	"os"
	"strings"

	"github.com/mralaminahamed/codetrail/packages/shared/config"
)

// redacted is what every rendering of a Secret says instead of the key.
const redacted = "[redacted]"

// Secret is an API key that cannot be printed by accident.
//
// This codebase had never held a secret before P7 — grep finds no API_KEY, no
// ANTHROPIC, no secrets manager outside a .gitignore comment — so there is no
// redaction helper to reuse and nothing else would catch a key in a log. A raw
// string here would be one fmt.Errorf("%w: %s", err, key) away from an
// aggregator.
//
// THESE THREE METHODS CLOSE DIRECT RENDERING ONLY. They cover %s, %v, %#v and
// json.Marshal OF A SECRET VALUE. They do NOT cover a Secret held in an
// unexported struct field, which is exactly how Anthropic holds it: fmt walks a
// struct by reflection, reflect.Value.CanInterface() is false for an unexported
// field, so fmt cannot call the Stringer and prints the underlying value.
// Measured on the shape this package ships:
//
//	nested %+v : {baseURL:https://api.anthropic.com key:{v:sk-CANARY-DO-NOT-LOG}}
//
// That is the line someone debugging a 401 writes. Anthropic therefore carries
// its own String and GoString; see anthropic.go.
type Secret struct{ v string }

// NewSecret wraps a key. There is no accessor: reveal is unexported and its
// only caller is the one place that sets the request header.
func NewSecret(v string) Secret { return Secret{v: v} }

func (s Secret) String() string { return redacted }

func (s Secret) GoString() string { return redacted }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

func (s Secret) empty() bool { return s.v == "" }

func (s Secret) reveal() string { return s.v }

// LoadSecret reads the key, from a file first and an environment variable
// second.
//
// A file path is what a secrets manager mounts, and P8 is where an AWS Secrets
// Manager client would belong — adding an SDK for it here would be the second
// unverified dependency in a phase that argued against the first.
//
// Never a .env file. Nothing in this codebase reads one (config is
// os.LookupEnv and nothing else), and the phase that introduces the project's
// first secret is the phase that owes .gitignore the pattern: .env alone does
// not match .env.local.
func LoadSecret() (Secret, error) {
	if path := config.Get("LLM_API_KEY_FILE", ""); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			// The path, never the contents: a read that half-succeeded must not
			// put a fragment of the key in an error.
			return Secret{}, fmt.Errorf("LLM_API_KEY_FILE=%s: %w", path, err)
		}
		// Trimmed, because a file written by `echo` ends in a newline and a key
		// with a trailing newline is a 401 nobody can see in a log.
		v := strings.TrimSpace(string(b))
		if v == "" {
			return Secret{}, fmt.Errorf("LLM_API_KEY_FILE=%s holds no key", path)
		}
		return Secret{v: v}, nil
	}
	if v := config.Get("LLM_API_KEY", ""); v != "" {
		return Secret{v: strings.TrimSpace(v)}, nil
	}
	return Secret{}, fmt.Errorf("no API key: set LLM_API_KEY_FILE or LLM_API_KEY, or set LLM_PROVIDER=none")
}
