.DEFAULT_GOAL := help
GO ?= go
COMPOSE ?= docker compose -f deploy/docker-compose.yml

# Pinned development tools are installed into ./bin and put first on PATH.
BIN := $(CURDIR)/bin
export PATH := $(BIN):$(PATH)

BUF_VERSION              ?= v1.73.0
PROTOC_GEN_GO_VERSION    ?= v1.36.12
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: tools
tools: ## Install pinned dev tools (buf, protoc plugins) into ./bin
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

.PHONY: test-short
test-short: ## Run fast tests only (skips Docker-based integration tests)
	$(GO) test -short -count=1 ./...

.PHONY: build
build: ## Build all packages
	$(GO) build ./...

.PHONY: up
up: ## Start local infrastructure (NATS JetStream, Postgres)
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
