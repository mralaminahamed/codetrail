# Contributing to codetrail

Thanks for taking the time to contribute. This guide covers the local setup, the checks a change has
to pass, and how branches, commits and pull requests are handled.

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md). Report security problems
privately as described in the [security policy](SECURITY.md), never in a public issue.

## Prerequisites

- Go 1.27
- Node 22 (for the console in `apps/console`)
- Docker with Compose v2
- `terraform` and `promtool`, only for the infrastructure targets

## Local setup

```bash
make up      # Postgres 17 with pgvector on localhost:55432
make build   # bin/gateway, bin/indexer, bin/evalrunner
make psql    # a shell against the running database
make down    # stop the compose stack
```

The console runs on its own:

```bash
cd apps/console && npm ci && npm run dev
```

The [README](README.md) has the full quickstart, the configuration reference and the build tags.

## Checks to run before opening a pull request

The CI workflow is currently disabled on GitHub, so nothing runs on your pull request. Run the gates
locally and say in the pull request which ones you ran.

Required for every change:

```bash
make lint    # gofmt + go vet, then the console's eslint and tsc
make test    # the hermetic Go suites and the console's
```

When the change touches the store, the queue, the binaries or the console fixtures, also run the
live suites against a real Postgres (`make up` first):

```bash
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' \
  go test -tags=live ./...
```

Run the checks that match what you changed:

```bash
make policy          # Terraform plan assertions (needs terraform)
make alerts-test     # Prometheus rules and config (needs promtool)
make images          # build both application images (needs Docker)
make image-test      # assert what is inside the images
make smoke           # end-to-end script against the built images over compose
make migrations-lint # refuse a destructive migration that is not expand-only
```

The `ollama` and `llm` build tags need a running Ollama or a paid API key; see the README before
running them. The `llm` suite costs money.

## Branches, commits and pull requests

- Branch from `trunk`.
- Keep commits small and single-scope, using [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat(console): ...`, `fix(indexer): ...`, `docs: ...`).
- One pull request per change, opened against `trunk`. Link the issue it addresses.
- Pull requests are merged with a merge commit, not squashed, so keep the commit history clean.
