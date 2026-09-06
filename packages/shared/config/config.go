// Package config reads service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Get reads a string setting, or def when it is unset OR EMPTY.
//
// Empty-is-unset is this package's house rule and it is stated here because it
// is a decision and not an implementation detail: a knob cleared in a compose
// file reads as one nobody set. It is the right rule for a value whose default
// is a fallback — a port, a URL, a model name — and the WRONG one for a value
// whose default is a permission, because clearing it then hands the permission
// back. See GetList, which is where that bit.
func Get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// GetList reads a comma-separated setting, or def when it is UNSET. Present
// and empty is an EMPTY LIST, which is where it parts company with Get.
//
// The difference is not a preference. ALLOWED_HOSTS is the only SSRF control
// codetrail has (admit's package doc), and read through Get it FAILED OPEN.
// Measured on the indexer's own allowedHosts:
//
//	ALLOWED_HOSTS=""   -> ["github.com" "codeberg.org"], github.com ADMITTED
//	ALLOWED_HOSTS=" "  -> [" "],                         github.com refused
//
// An operator clearing the control got the built-in allowlist back, and a
// single space was the difference between fail-open and fail-closed. Clearing
// a permission means "grant nothing", never "grant the default".
//
// Whitespace-only is that same empty list rather than a one-entry list holding
// a space, so the two spellings above answer alike at this layer instead of
// alike only because admit.NewPolicy happens to trim. Splitting is left
// unnormalised otherwise: the caller knows whether its entries are hosts.
func GetList(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// GetInt reads an integer setting, or def when it is unset or empty.
//
// A value that is not an integer is an error rather than the default:
// RETRIEVAL_CANDIDATES=4O with a letter O ran at 40 and said nothing, which is
// a setting an operator believes is in force and is not. Every caller here
// already refuses a value out of range; this closes the case where the value
// never became a number at all.
func GetInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, v)
	}
	return n, nil
}

func MustGet(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		panic("missing required env: " + key)
	}
	return v
}
