.DEFAULT_GOAL := help
GO ?= go
COMPOSE ?= docker compose -f deploy/docker-compose.yml

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

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
test: ## Run unit tests with the race detector
	$(GO) test -race -cover ./...

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
