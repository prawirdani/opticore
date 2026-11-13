# Opticore development tasks. Configuration comes from .env.

ENV_FILE ?= .env
BIN      ?= bin/opticore
TEST_DSN ?= postgres://postgres:pgdocker1337@localhost:5432/opticore_test?sslmode=disable
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUMP     := $(filter major minor patch,$(MAKECMDGOALS))

.PHONY: help dev serve build migrate test itest fmt vet lint tidy clean db-test release major minor patch

help: ## list targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  %-10s %s\n", $$1, $$2}'

dev: ## run the daemon with hot reload (air)
	@command -v air >/dev/null || { echo "install: go install github.com/air-verse/air@latest"; exit 1; }
	set -a; . ./$(ENV_FILE); set +a; air

serve: ## run the daemon once
	set -a; . ./$(ENV_FILE); set +a; go run ./cmd/opticore serve

build: ## build the binary (version from git describe)
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/opticore

migrate: build ## apply migrations
	set -a; . ./$(ENV_FILE); set +a; $(BIN) migrate

db-test: ## create the test database if absent
	@psql "$(TEST_DSN)" -c 'select 1' >/dev/null 2>&1 || \
	  psql "$${TEST_DSN%%/opticore_test*}/postgres" -c 'create database opticore_test'

test: ## unit tests (no database)
	go test ./...

itest: ## tests including PostgreSQL integration
	OPTICORE_TEST_DSN='$(TEST_DSN)' go test -race -count=1 ./...

fmt: ## format
	gofmt -l -w .

vet: ## go vet
	go vet ./...

lint: vet ## vet plus staticcheck if installed
	@command -v staticcheck >/dev/null && staticcheck ./... || true

tidy: ## tidy modules
	go mod tidy

clean: ## remove build output
	rm -rf bin tmp

release: ## tag a release: make release major|minor|patch
	@set -eu; \
	if [ -z "$(BUMP)" ]; then echo "usage: make release major|minor|patch"; exit 1; fi; \
	if [ -n "$$(git status --porcelain)" ]; then echo "release: working tree not clean"; exit 1; fi; \
	last=$$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || echo v0.0.0); \
	v=$${last#v}; major=$${v%%.*}; rest=$${v#*.}; minor=$${rest%%.*}; patch=$${rest#*.}; \
	case "$(BUMP)" in \
	  major) major=$$((major + 1)); minor=0; patch=0 ;; \
	  minor) minor=$$((minor + 1)); patch=0 ;; \
	  patch) patch=$$((patch + 1)) ;; \
	esac; \
	next="v$$major.$$minor.$$patch"; \
	git tag -a "$$next" -m "release $$next"; \
	echo "tagged $$next (was $$last)"; \
	echo "publish: git push origin $$next"

# Arguments to release; they are not targets of their own.
major minor patch:
	@:
