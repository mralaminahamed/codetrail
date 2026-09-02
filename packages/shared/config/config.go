// Package config reads service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
)

func Get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
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
