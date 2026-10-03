.DEFAULT_GOAL := help
GO ?= go
COMPOSE ?= docker compose -f deploy/docker-compose.yml

# Pinned development tools are installed into ./bin and put first on PATH.
BIN := $(CURDIR)/bin
export PATH := $(BIN):$(PATH)

BUF_VERSION              ?= v1.73.0
PROTOC_GEN_GO_VERSION    ?= v1.36.12
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2
SQLC_VERSION             ?= v1.31.1

# Services that have a database (each has internal/adapters/postgres/sqlc.yaml).
SQLC_SERVICES := inventory payment order notification
SERVICE ?= inventory

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: tools
tools: ## Install pinned dev tools (buf, protoc plugins, sqlc) into ./bin
	GOBIN=$(BIN) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	GOBIN=$(BIN) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	GOBIN=$(BIN) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(BIN) $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

.PHONY: proto
proto: ## Regenerate Go code from api/proto into gen/
	buf generate

.PHONY: proto-lint
proto-lint: ## Lint protobuf files
	buf lint api/proto

.PHONY: proto-breaking
proto-breaking: ## Check protos for breaking changes against main
	buf breaking api/proto --against '.git#branch=main,subdir=api/proto'

.PHONY: sqlc
sqlc: ## Regenerate type-safe query code for every service
	@for s in $(SQLC_SERVICES); do (cd services/$$s/internal/adapters/postgres && sqlc generate) || exit 1; done

.PHONY: docker-build
docker-build: ## Build a service image: make docker-build SERVICE=inventory
	docker build -f deploy/docker/Dockerfile --build-arg SERVICE=$(SERVICE) -t $(SERVICE)-service:dev .

.PHONY: tidy
tidy: ## Sync go.mod and go.sum
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Format code
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint (must be installed)
	golangci-lint run ./...

.PHONY: test
test: ## Run all tests with the race detector (integration tests need Docker)
	$(GO) test -race -count=1 -cover ./...

.PHONY: test-race-docker
test-race-docker: ## Run the short tests under the race detector in a Linux container (for machines without a C toolchain, e.g. Windows)
	docker run --rm -v "$(CURDIR):/src" -v gomodcache:/go/pkg/mod -w /src -e GOFLAGS=-buildvcs=false golang:1.26 go test -race -short -count=1 ./...

.PHONY: test-short
test-short: ## Run fast tests only (skips Docker-based integration tests)
	$(GO) test -short -count=1 ./...

.PHONY: build
build: ## Build all packages
	$(GO) build ./...

.PHONY: e2e
e2e: ## Run end-to-end tests against the full stack (builds images, needs Docker)
	$(COMPOSE) up -d --build
	$(GO) test -tags e2e -count=1 ./test/e2e/... ; status=$$?; $(COMPOSE) down -v; exit $$status

.PHONY: rules-test
rules-test: ## Check the Prometheus config and unit-test the alert rules (needs Docker)
	docker run --rm -v "$(CURDIR)/deploy/prometheus:/etc/prometheus" --entrypoint promtool prom/prometheus:v2.55.1 check config /etc/prometheus/prometheus.yml
	docker run --rm -v "$(CURDIR)/deploy/prometheus:/etc/prometheus" -w /etc/prometheus --entrypoint promtool prom/prometheus:v2.55.1 test rules rules_test.yml

.PHONY: up
up: ## Start the whole stack: infrastructure, services and monitoring
	$(COMPOSE) up -d

.PHONY: down
down: ## Stop local infrastructure
	$(COMPOSE) down

.PHONY: reset
reset: ## Stop infrastructure and delete its volumes
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail infrastructure logs
	$(COMPOSE) logs -f
