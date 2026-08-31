// Package config reads service configuration from the environment.
package config

import (
	"os"
	"strconv"
)

func Get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func GetInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func MustGet(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		panic("missing required env: " + key)
	}
	return v
}
