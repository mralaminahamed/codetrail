// Package logger builds service-scoped zerolog loggers.
package logger

import (
	"os"

	"github.com/rs/zerolog"
)

func New(service string) zerolog.Logger { return build(os.Stdout, service) }

// NewStderr keeps stdout clean for stdio protocols (e.g. MCP).
func NewStderr(service string) zerolog.Logger { return build(os.Stderr, service) }

func build(w *os.File, service string) zerolog.Logger {
	return zerolog.New(w).With().Timestamp().Str("svc", service).Logger().
		Level(zerolog.InfoLevel)
}
