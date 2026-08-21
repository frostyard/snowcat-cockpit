SHELL := /usr/bin/env bash
GO ?= go
GOCACHE ?= /tmp/snowcat-cockpit-gocache

.PHONY: build ci fmt-check test test-go test-observer-wrapper test-spike vet

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
