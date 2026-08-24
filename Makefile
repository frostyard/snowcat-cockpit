SHELL := /usr/bin/env bash
GO ?= go
GOCACHE ?= /tmp/snowcat-cockpit-gocache
GOFMT ?= gofmt

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo development)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_TIME) -X main.builtBy=make"

.PHONY: build build-cross bump check ci clean docs-check docker-image fmt fmt-check install lint lint-version-check oci-image test test-cover test-go test-oci-entrypoints test-observer-wrapper test-race test-spike tidy-check verify vet

# Pinned golangci-lint release, read from mise.toml — the single source of
# every tool pin (core ADR-0043): `mise install` provisions it locally, in CI
# (jdx/mise-action), and on Snowcat workers, verified against mise.lock.
# Bump it there in a dedicated commit; never edit this line.
GOLANGCI_LINT_VERSION := $(strip $(shell sed -n 's/^golangci-lint = "\(.*\)"/\1/p' mise.toml))
# The Go release this module is built with, from go.mod's toolchain line —
# the only Go pin (mise reads the same line). golangci-lint must be built
# with a Go at least this new, or its embedded gofmt and typechecker disagree
# with the toolchain.
GO_TOOLCHAIN := $(strip $(shell sed -n 's/^toolchain go\(.*\)/\1/p' go.mod))

check: ci

# verify: credential-free, non-mutating gate (what a worker and a read-only
# reviewer run); ci adds race, cross-build, and docs-check.
verify: tidy-check fmt-check vet lint-version-check lint test

ci: verify test-race build-cross docs-check

tidy-check:
	$(GO) mod tidy -diff

fmt:
	$(GOFMT) -w $$(git ls-files '*.go')

fmt-check:
	test -z "$$(gofmt -l cmd internal)"

vet:
	GOCACHE=$(GOCACHE) $(GO) vet ./...

lint-version-check:
	@test -n "$(GOLANGCI_LINT_VERSION)" || { echo "mise.toml pins no golangci-lint (add it, then run: mise install)"; exit 1; }
	@installed="$$(golangci-lint version --short 2>/dev/null)" || { \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) is required for make ci (not installed; run: mise install)"; exit 1; }; \
	if [[ "$$installed" != "$(GOLANGCI_LINT_VERSION)" ]]; then \
		echo "expected golangci-lint $(GOLANGCI_LINT_VERSION), found $$installed (run: mise install)"; exit 1; \
	fi; \
	built="$$(golangci-lint version 2>/dev/null | sed -n 's/.*built with go\([0-9.]*\).*/\1/p')"; \
	if [[ -n "$$built" ]] && [[ "$$(printf '%s\n%s\n' "$(GO_TOOLCHAIN)" "$$built" | sort -V | head -1)" != "$(GO_TOOLCHAIN)" ]]; then \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) was built with go$$built, older than go.mod's toolchain go$(GO_TOOLCHAIN): bump golangci-lint first (core ADR-0043)"; exit 1; \
	fi

lint:
	golangci-lint run

docs-check:
	node scripts/check-docs.mjs

test: test-go test-observer-wrapper test-oci-entrypoints test-spike

test-go:
	GOCACHE=$(GOCACHE) $(GO) test ./...

test-cover:
	GOCACHE=$(GOCACHE) $(GO) test -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

test-race:
	GOCACHE=$(GOCACHE) $(GO) test -race -short ./...

test-observer-wrapper:
	bash -n bin/snowcat-cockpit-serve oci/entrypoint.sh oci/claude-entrypoint.sh oci/copilot-entrypoint.sh test/observer-wrapper.test.sh
	./test/observer-wrapper.test.sh

test-oci-entrypoints:
	bash -n test/oci-entrypoints.test.sh
	./test/oci-entrypoints.test.sh

test-spike:
	bash -n bin/snowcat-cockpit test/cockpit.test.sh
	./test/cockpit.test.sh

build:
	mkdir -p dist
	GOCACHE=$(GOCACHE) $(GO) build -trimpath $(LDFLAGS) -o dist/snowcat-cockpit ./cmd/snowcat-cockpit

build-cross:
	GOOS=linux GOARCH=amd64 GOCACHE=$(GOCACHE) $(GO) build -trimpath -o /tmp/snowcat-cockpit-linux-amd64 ./cmd/snowcat-cockpit
	GOOS=linux GOARCH=arm64 GOCACHE=$(GOCACHE) $(GO) build -trimpath -o /tmp/snowcat-cockpit-linux-arm64 ./cmd/snowcat-cockpit

install:
	$(GO) install -trimpath $(LDFLAGS) ./cmd/snowcat-cockpit

clean:
	rm -rf -- dist coverage.out coverage.html

bump:
	$(MAKE) ci
	@if [[ -n "$$(git status --porcelain)" ]]; then echo "working tree is not clean"; exit 1; fi
	@version="$$(svu next)"; \
	git tag -a "$$version" -m "Version $$version"; \
	git push origin "$$version"

oci-image: oci-image-codex oci-image-claude oci-image-copilot

# One Containerfile, one base, three provider targets (ADR-0012 §1).
PROVIDERS := codex claude copilot
define provider-image-rules
oci-image-$(1):
	podman build --build-arg TARGETARCH="$$$$(go env GOARCH)" --file oci/Containerfile --target $(1) --tag localhost/snowcat-cockpit-worker:$(1) .
	@podman image inspect localhost/snowcat-cockpit-worker:$(1) --format 'export SNOWCAT_COCKPIT_OCI_$(shell echo $(1) | tr a-z A-Z)_IMAGE=sha256:{{.Id}}'

docker-image-$(1):
	docker build --build-arg TARGETARCH="$$$$(go env GOARCH)" --file oci/Containerfile --target $(1) --tag localhost/snowcat-cockpit-worker:$(1) .
	@docker image inspect localhost/snowcat-cockpit-worker:$(1) --format 'export SNOWCAT_COCKPIT_DOCKER_$(shell echo $(1) | tr a-z A-Z)_IMAGE={{.Id}}'
endef
$(foreach p,$(PROVIDERS),$(eval $(call provider-image-rules,$(p))))

docker-image: docker-image-codex docker-image-claude docker-image-copilot

