BIN := bin

.PHONY: build gateway indexer lint test up down psql images image-test alerts-test tf-check tf-plan policy

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

alerts-test:
	promtool check rules infra/prometheus/alerts.yml
	promtool check config infra/prometheus/prometheus.compose.yml infra/prometheus/prometheus.aws.yml
	promtool test rules infra/prometheus/alerts_test.yml
	./infra/prometheus/metric_names.sh
