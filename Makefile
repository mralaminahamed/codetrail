BIN := bin

.PHONY: build gateway indexer evalrunner lint test up down psql eval-corpus

DATABASE_URL ?= postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable
OLLAMA_URL ?= http://localhost:11435
export DATABASE_URL OLLAMA_URL

build: gateway indexer evalrunner

gateway:
	go build -o $(BIN)/gateway ./apps/gateway/cmd

indexer:
	go build -o $(BIN)/indexer ./apps/indexer/cmd

evalrunner:
	go build -o $(BIN)/evalrunner ./apps/evalrunner/cmd

lint:
	@test -z "$$(gofmt -l apps packages)" || { gofmt -l apps packages; exit 1; }
	go vet ./...

test:
	go test ./...

up:
	docker compose -f infra/docker-compose.yml up -d

down:
	docker compose -f infra/docker-compose.yml down

psql:
	docker compose -f infra/docker-compose.yml exec postgres psql -U codetrail -d codetrail

# The two indexer passes spec §9's comparison needs, as one command rather than
# a paragraph. One arm per database: RepoID = hash(key, commit) has no room for
# a strategy, so a second arm in the first arm's database replaces it.
#
# Capture each pass's log line: vanished, unstrippable, tokenless and unparsed
# are in no table, and the artefact cannot get them any other way.
EVAL_AST_URL ?= postgres://codetrail:codetrail@localhost:55432/codetrail_eval_ast?sslmode=disable
EVAL_WINDOW_URL ?= postgres://codetrail:codetrail@localhost:55432/codetrail_eval_window?sslmode=disable

eval-corpus: indexer
	CHUNK_STRATEGY=ast    STRIP_DOC_COMMENTS=true DATABASE_URL=$(EVAL_AST_URL)    $(BIN)/indexer
	CHUNK_STRATEGY=window STRIP_DOC_COMMENTS=true DATABASE_URL=$(EVAL_WINDOW_URL) $(BIN)/indexer
