SHELL := /bin/sh
GO := $(if $(wildcard .build/tools/go/bin/go),$(CURDIR)/.build/tools/go/bin/go,go)
export GOCACHE := $(CURDIR)/.build/go-cache
export GOPATH := $(CURDIR)/.build/gopath
export PATH := $(firstword $(wildcard $(CURDIR)/.build/tools/node-v26.13.0-*/bin)):$(PATH)
export COSMOFORMER_TEST_OWNER_DSN ?= $(shell cat .build/testdb/owner.dsn 2>/dev/null)
export COSMOFORMER_TEST_MARKER_FILE ?= $(CURDIR)/.build/testdb/marker

.PHONY: brand tools deps dev-setup dev-api dev-web dev-stop contract test-db-start test-db-stop test-unit test-integration test-coverage test-go-coverage test-web-unit test-web-coverage test-web browser-server check

brand:
	node scripts/brand.mjs

tools:
	node scripts/tools.mjs

deps:
	$(GO) mod download
	cd web && npm ci --ignore-scripts

contract:
	cd web && COSMOFORMER_GO=$(GO) npm run contract

dev-setup:
	node scripts/dev.mjs setup

dev-api:
	COSMOFORMER_LISTEN=127.0.0.1:18080 COSMOFORMER_ORIGIN=https://localhost:5173 COSMOFORMER_DATABASE_DSN_FILE=$(CURDIR)/.build/dev/app.dsn $(GO) run ./cmd/cosmoformer serve

dev-web: contract
	cd web && npm run dev

dev-stop:
	node scripts/dev.mjs stop

test-db-start:
	node scripts/dev.mjs test-start

test-db-stop:
	node scripts/dev.mjs test-stop

test-unit:
	$(GO) test -race ./internal/... ./cmd/...
	$(GO) vet ./...

test-integration:
	$(GO) test -race -tags=integration ./internal/store ./cmd/... ./tests/acceptance

test-coverage: test-go-coverage test-web-coverage

# Includes acceptance calls in the same profile as unit tests; never use the dev DB.
test-go-coverage:
	mkdir -p .build/coverage
	@set -eu; cover_dir=$$(mktemp -d "$(CURDIR)/.build/coverage/go-raw.XXXXXX"); \
	trap 'rm -rf "$$cover_dir"' EXIT; \
	COSMOFORMER_TEST_COVER_DIR="$$cover_dir" $(GO) test -race -count=1 -tags=integration -covermode=atomic -coverpkg=./internal/...,./cmd/... ./internal/... ./cmd/... ./tests/acceptance -args -test.gocoverdir="$$cover_dir"; \
	$(GO) tool covdata textfmt -i="$$cover_dir" -o=.build/coverage/go.out
	$(GO) tool cover -func=.build/coverage/go.out
	$(GO) tool cover -html=.build/coverage/go.out -o=.build/coverage/go.html

test-web-unit: contract
	cd web && npm run test:unit

test-web-coverage: contract
	cd web && npm run test:coverage

browser-server:
	COSMOFORMER_TEST_BROWSER=1 $(GO) test -count=1 -tags=integration ./tests/acceptance -run '^TestBrowserServer$$' -timeout 10m -v

test-web: contract
	mkdir -p .build/browser-tmp
	cd web && npm run check && npm run test:coverage && npm run build && TMPDIR=$(CURDIR)/.build/browser-tmp npm test

# These are the checks for the implemented account/access slice. Native engine
# tests arrive with execution support; no worker or cloud path exists here.
check:
	$(MAKE) test-unit
	$(MAKE) test-integration
	$(MAKE) test-web
