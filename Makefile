.PHONY: help fmt tidy vet build citadel test test-integration verify stack-up stack-down stack-verify pin-check

GO ?= go
COMPOSE_DIR := deploy/compose

# The build identity of SDD section 4.1. A plain go build keeps the placeholders
# that cmd/citadel declares, so only a make build carries real values.
CITADEL_PKG := github.com/andersonten/simcitadel/cmd/citadel
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildDate=$(BUILD_DATE)
# The package pattern keeps the -X symbols off every other main package.
CITADEL_LDFLAGS := "-ldflags=$(CITADEL_PKG)=$(LDFLAGS)"

help:
	@echo "verify           Run the full gate: fmt, vet, build, unit tests"
	@echo "citadel          Build the citadel binary with the build identity"
	@echo "test-integration Run the container-backed tests of SDD 14.2"
	@echo "stack-up         Start the compose stack and wait for every service"
	@echo "stack-verify     Start the stack, assert every service is healthy, then stop it"
	@echo "pin-check        Assert every compose image digest matches the registry"

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

tidy:
	$(GO) mod tidy -diff

vet:
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...

build:
	$(GO) build $(CITADEL_LDFLAGS) ./...

citadel:
	$(GO) build $(CITADEL_LDFLAGS) -o citadel ./cmd/citadel

test:
	$(GO) test ./...

# The integration suite starts its own containers, so it needs a Docker daemon.
test-integration:
	$(GO) test -tags=integration -timeout 30m ./internal/integration/...

verify: fmt tidy vet build test

stack-up:
	cd $(COMPOSE_DIR) && docker compose up -d --wait --wait-timeout 300

stack-down:
	cd $(COMPOSE_DIR) && docker compose down -v --remove-orphans

# stack-verify fails when any service ends in a state other than healthy or
# a clean exit, which is what a first-run failure looks like.
stack-verify: stack-up
	@cd $(COMPOSE_DIR) && bad=$$(docker compose ps -a --format '{{.Service}} {{.Status}}' \
		| grep -Ev '\(healthy\)|Exited \(0\)' || true); \
	if [ -n "$$bad" ]; then echo "unhealthy service:"; echo "$$bad"; exit 1; fi; \
	echo "every compose service is healthy"
	$(MAKE) stack-down

pin-check:
	./$(COMPOSE_DIR)/pin-images.sh
