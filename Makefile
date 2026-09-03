BIN := bin

.PHONY: build gateway indexer lint test up down psql images image-test

DATABASE_URL ?= postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable
OLLAMA_URL ?= http://localhost:11435
export DATABASE_URL OLLAMA_URL

build: gateway indexer

gateway:
	go build -o $(BIN)/gateway ./apps/gateway/cmd

indexer:
	go build -o $(BIN)/indexer ./apps/indexer/cmd

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

# The build context is the repository root, which is what a Go module build
# needs and what .dockerignore is written against.
images:
	docker build -f infra/gateway.Dockerfile -t codetrail/gateway:test .
	docker build -f infra/indexer.Dockerfile -t codetrail/indexer:test .

image-test:
	./infra/image_test.sh
