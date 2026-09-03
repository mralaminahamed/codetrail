BIN := bin

# Go's tooling does NOT skip node_modules. Measured on go1.27.0 with
# apps/console/node_modules present: `go list ./...` includes
# apps/console/node_modules/flatted/golang/pkg/flatted (ESLint's flat-cache
# pulls flatted, which ships a Go port). So both Go targets are scoped by what
# git tracks rather than by directory. CI is unaffected either way: the go job
# never runs npm ci, so node_modules is not on that runner's disk.
GOFILES := $(shell git ls-files '*.go')
GOPKGS := $(shell git ls-files '*.go' | xargs -n1 dirname | sort -u | sed 's|^|./|')

.PHONY: build gateway indexer lint test console-lint console-test up down psql

DATABASE_URL ?= postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable
OLLAMA_URL ?= http://localhost:11435
export DATABASE_URL OLLAMA_URL

build: gateway indexer

gateway:
	go build -o $(BIN)/gateway ./apps/gateway/cmd

indexer:
	go build -o $(BIN)/indexer ./apps/indexer/cmd

lint: console-lint
	@test -z "$$(gofmt -l $(GOFILES))" || { gofmt -l $(GOFILES); exit 1; }
	go vet $(GOPKGS)

test: console-test
	go test $(GOPKGS)

apps/console/node_modules:
	cd apps/console && npm ci

console-lint: apps/console/node_modules
	cd apps/console && npm run lint && npm run typecheck

console-test: apps/console/node_modules
	cd apps/console && npm run test -- --run

up:
	docker compose -f infra/docker-compose.yml up -d

down:
	docker compose -f infra/docker-compose.yml down

psql:
	docker compose -f infra/docker-compose.yml exec postgres psql -U codetrail -d codetrail
