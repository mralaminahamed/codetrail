package main

import (
	"context"

	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// storeHandle is the store surface main needs. Named here rather than in
// store.go so the readiness composition test can substitute a fake without a
// Postgres to connect to.
type storeHandle interface {
	Ping(context.Context) error
	Close()
}

// openStore is a seam: main calls it, tests replace it.
var openStore = func(ctx context.Context, dsn string) (storeHandle, error) {
	return store.New(ctx, dsn)
}
