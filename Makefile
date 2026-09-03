BIN := bin

.PHONY: build gateway indexer evalrunner lint test up down psql eval-corpus images image-test alerts-test tf-check tf-plan policy smoke migrations-lint

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

# The build context is the repository root, which is what a Go module build
# needs and what .dockerignore is written against.
images:
	docker build -f infra/gateway.Dockerfile -t codetrail/gateway:test .
	docker build -f infra/indexer.Dockerfile -t codetrail/indexer:test .

image-test:
	./infra/image_test.sh

# Both scrape files, because they share one rule_files entry and a rule
# selecting service="indexer" against a scrape file that never sets the label
# is a rule that silently matches nothing.
tf-check:
	terraform fmt -check -recursive infra/terraform
	cd infra/terraform && terraform init -backend=false -input=false >/dev/null && terraform validate

# No AWS account, no credentials that resolve to anything. See the script.
tf-plan:
	./infra/terraform/offline_plan.sh

policy: tf-plan
	go vet -tags=tfplan ./infra/...
	go test -tags=tfplan -count=1 ./infra/policy/

migrations-lint:
	./infra/migrations_lint.sh

smoke:
	./infra/smoke.sh --target compose

alerts-test:
	promtool check rules infra/prometheus/alerts.yml
	promtool check config infra/prometheus/prometheus.compose.yml infra/prometheus/prometheus.aws.yml
	promtool test rules infra/prometheus/alerts_test.yml
	./infra/prometheus/metric_names.sh
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
