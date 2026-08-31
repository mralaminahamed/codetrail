package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// storeHandle is the store surface main needs: a ping for readiness, and the
// pool the job queue is built from. A test stands in for it without a database:
// pgxpool.New does not dial, so a fake hands back a real pool aimed at an
// address nothing answers on and the queue errors rather than panicking. That
// Pool returns a concrete *pgxpool.Pool does not stop this — see deadStore.
type storeHandle interface {
	Ping(context.Context) error
	Pool() *pgxpool.Pool
	Close()
}

// openStore is a seam: main calls it, tests replace it.
var openStore = func(ctx context.Context, dsn string) (storeHandle, error) {
	return store.New(ctx, dsn)
}
