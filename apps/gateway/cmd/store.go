package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// storeHandle is the store surface main needs: a ping for readiness, and the
// pool the job queue is built from. Pool returns a concrete *pgxpool.Pool, so
// this narrows *store.Store rather than being a seam a fake can stand in for.
type storeHandle interface {
	Ping(context.Context) error
	Pool() *pgxpool.Pool
	Close()
}

// openStore is a seam: main calls it, tests replace it.
var openStore = func(ctx context.Context, dsn string) (storeHandle, error) {
	return store.New(ctx, dsn)
}
