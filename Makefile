SHELL := /usr/bin/env bash
GO ?= go
GOCACHE ?= /tmp/snowcat-cockpit-gocache

.PHONY: build ci fmt-check oci-image oci-image-claude oci-image-codex oci-image-copilot test test-go test-oci-entrypoints test-observer-wrapper test-spike vet

ci: fmt-check vet test

fmt-check:
	test -z "$$(gofmt -l cmd internal)"

vet:
	GOCACHE=$(GOCACHE) $(GO) vet ./...

test: test-go test-observer-wrapper test-oci-entrypoints test-spike

test-go:
	GOCACHE=$(GOCACHE) $(GO) test ./...

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
	GOCACHE=$(GOCACHE) $(GO) build -trimpath -o dist/snowcat-cockpit ./cmd/snowcat-cockpit

oci-image: oci-image-codex oci-image-claude oci-image-copilot

oci-image-codex:
	podman build --file oci/Containerfile --tag localhost/snowcat-cockpit-worker:codex-0.149.0 .
	@podman image inspect localhost/snowcat-cockpit-worker:codex-0.149.0 --format 'export SNOWCAT_COCKPIT_OCI_CODEX_IMAGE=sha256:{{.Id}}'

oci-image-claude:
	podman build --build-arg TARGETARCH="$$(go env GOARCH)" --file oci/Claude.Containerfile --tag localhost/snowcat-cockpit-worker:claude-2.1.239 .
	@podman image inspect localhost/snowcat-cockpit-worker:claude-2.1.239 --format 'export SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE=sha256:{{.Id}}'

oci-image-copilot:
	podman build --build-arg TARGETARCH="$$(go env GOARCH)" --file oci/Copilot.Containerfile --tag localhost/snowcat-cockpit-worker:copilot-1.0.80 .
	@podman image inspect localhost/snowcat-cockpit-worker:copilot-1.0.80 --format 'export SNOWCAT_COCKPIT_OCI_COPILOT_IMAGE=sha256:{{.Id}}'
