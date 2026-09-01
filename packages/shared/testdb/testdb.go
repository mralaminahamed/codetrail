// Package testdb creates the throwaway databases the live suites run against.
//
// A suite that clears whole tables must not clear a database it did not
// create. The README tells a reader to run `go test -tags=live ./...` against
// the DSN `make up` serves, and measured before this existed: a seeded repo,
// its 51 spans and a job all vanished from that database while the suite went
// green — `DELETE FROM repos` with no WHERE on entry and on exit, and the same
// to `jobs`.
//
// A normal package rather than a _test.go helper because three suites need it
// — store, jobs and the indexer — and a test file's symbols cannot be shared.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Scratch creates a database beside the one base names, hands run its DSN, and
// drops it again. base itself is only ever connected to in order to issue those
// two statements, so nothing a suite does inside run can reach it.
//
// It returns rather than exiting so a caller's defers run before TestMain's
// os.Exit.
func Scratch(base, prefix string, run func(dsn string) int) (int, error) {
	ctx := context.Background()
	u, err := url.Parse(base)
	if err != nil {
		return 0, fmt.Errorf("DATABASE_URL is not a URL: %w", err)
	}
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	defer admin.Close(ctx)

	name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return 0, fmt.Errorf("create %s: %w", name, err)
	}
	defer func() {
		// FORCE, because a pool that outlived a failing test still holds a
		// session and DROP DATABASE would block on it.
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			// Not fatal: the tests have already run and their verdict is what
			// the caller is returning. A leaked database is visible in \l.
			fmt.Fprintf(os.Stderr, "testdb: dropping %s: %v\n", name, err)
		}
	}()

	u.Path = "/" + name
	return run(u.String()), nil
}

// Name is the database a DSN points at, which is what a suite compares against
// to prove it is not running in the one it was pointed at.
func Name(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}
