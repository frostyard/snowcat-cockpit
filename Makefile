SHELL := /usr/bin/env bash
GO ?= go
GOCACHE ?= /tmp/snowcat-cockpit-gocache

.PHONY: build ci fmt-check oci-image test test-go test-observer-wrapper test-spike vet

ci: fmt-check vet test

fmt-check:
	test -z "$$(gofmt -l cmd internal)"

vet:
	GOCACHE=$(GOCACHE) $(GO) vet ./...

test: test-go test-observer-wrapper test-spike

test-go:
	GOCACHE=$(GOCACHE) $(GO) test ./...

test-observer-wrapper:
	bash -n bin/snowcat-cockpit-serve test/observer-wrapper.test.sh
	./test/observer-wrapper.test.sh

test-spike:
	bash -n bin/snowcat-cockpit test/cockpit.test.sh
	./test/cockpit.test.sh

build:
	mkdir -p dist
	GOCACHE=$(GOCACHE) $(GO) build -trimpath -o dist/snowcat-cockpit ./cmd/snowcat-cockpit

oci-image:
	podman build --file oci/Containerfile --tag localhost/snowcat-cockpit-worker:codex-0.149.0 .
	@podman image inspect localhost/snowcat-cockpit-worker:codex-0.149.0 --format 'export SNOWCAT_COCKPIT_OCI_IMAGE=sha256:{{.Id}}'
