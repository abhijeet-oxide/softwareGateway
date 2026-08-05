# softwareGateway — see docs/design/14-deployment-and-development.md section 5.3

SHELL := /bin/bash
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

MODULE  := github.com/abhijeet-oxide/softwareGateway
LDFLAGS := -s -w \
	-X $(MODULE)/internal/platform/version.Version=$(VERSION) \
	-X $(MODULE)/internal/platform/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/platform/version.Date=$(DATE)

BINARIES := coordinator worker transferctl
DEV_CONFIG := ./dev/config.yaml

# Windows needs the .exe suffix, and `go build -o <name>` does NOT add it —
# Go only appends .exe when -o is omitted or names a directory. Without this,
# a Windows build produces `bin/transferctl` with no extension, which the shell
# refuses to execute and which you then have to rename by hand.
#
# GOOS is asked of the toolchain rather than guessed from the host, so
# `GOOS=windows make build` cross-compiles correctly from Linux or macOS too.
GOOS ?= $(shell go env GOOS)
ifeq ($(GOOS),windows)
EXE := .exe
else
EXE :=
endif

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- build ----

# Every Go source, the module files, and the embedded SQL. Without these
# prerequisites `bin/coordinator` is a file target with no dependencies, so
# make considers it up to date forever and `make build` silently does nothing
# after the first run — leaving you testing a stale binary.
SOURCES := $(shell find cmd internal pkg db -type f \( -name '*.go' -o -name '*.sql' \) 2>/dev/null) go.mod go.sum

.PHONY: build
build: $(addsuffix $(EXE),$(addprefix bin/,$(BINARIES))) ## Build all three binaries into bin/

bin/%$(EXE): $(SOURCES)
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/$*

.PHONY: build-all
build-all: ## Cross-compile every binary for linux, darwin and windows (amd64 + arm64)
	@for os in linux darwin windows; do \
		for arch in amd64 arm64; do \
			ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
			out="dist/$$os-$$arch"; mkdir -p $$out; \
			for b in $(BINARIES); do \
				echo "  $$os/$$arch  $$b$$ext"; \
				CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
					go build -trimpath -ldflags="$(LDFLAGS)" -o $$out/$$b$$ext ./cmd/$$b || exit 1; \
			done; \
		done; \
	done
	@echo "binaries in dist/"

.PHONY: clean
clean: ## Remove build output and the local database
	rm -rf bin/ dist/ dev/swgw.db dev/swgw.db-shm dev/swgw.db-wal coverage.out

## ----------------------------------------------------------------- test ----

# NOTE: unit tests MUST NOT require Docker. They run against an in-memory
# registry and SQLite, because a suite that needs containers is a suite
# developers run less often — and the difference compounds.
.PHONY: test
test: ## Run unit tests (no Docker required)
	go test -race ./...

.PHONY: test-short
test-short: ## Run unit tests without the race detector
	go test ./...

.PHONY: cover
cover: ## Run tests with coverage and print a summary
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: test-integration
test-integration: ## Run integration tests (requires Docker)
	go test -tags=integration ./test/integration/...

## ----------------------------------------------------------------- lint ----

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w ./cmd ./internal ./pkg

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy and verify module dependencies
	go mod tidy
	go mod verify

.PHONY: check
check: fmt vet lint test ## Everything CI runs

## ------------------------------------------------------------------ dev ----

.PHONY: dev-coordinator
dev-coordinator: ## Run the Coordinator against SQLite (zero setup)
	go run ./cmd/coordinator --config $(DEV_CONFIG)

.PHONY: dev-worker
dev-worker: ## Run a Worker
	go run ./cmd/worker --config $(DEV_CONFIG)

.PHONY: dev-postgres
dev-postgres: ## Start PostgreSQL for development
	docker compose up -d postgres
	@echo "SWGW_DATABASE_DRIVER=postgres SWGW_DATABASE_DSN='postgres://swgw:swgw@localhost:5432/swgw?sslmode=disable' make dev-coordinator"

.PHONY: dev-registry
dev-registry: ## Start two local OCI registries (source and destination)
	docker compose up -d registry registry-dest
	@echo "source      localhost:5000"
	@echo "destination localhost:5001"

.PHONY: dev-down
dev-down: ## Stop all development containers
	docker compose down -v

.PHONY: validate
validate: ## Validate the sample product configuration
	go run ./cmd/transferctl config validate ./dev/products

## -------------------------------------------------------------- database ----

.PHONY: migrate-status
migrate-status: ## Show migration status against the dev database
	go run ./cmd/coordinator --config $(DEV_CONFIG) --version >/dev/null
	@echo "migrations are applied automatically at Coordinator startup"

## --------------------------------------------------------------- docker ----

.PHONY: docker
docker: ## Build all container images
	for b in $(BINARIES); do \
		docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
			-f build/Dockerfile.$$b -t softwaregateway-$$b:$(VERSION) . || exit 1; \
	done
