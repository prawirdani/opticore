# Opticore development tasks. Configuration comes from .env.

ENV_FILE ?= .env
BIN      ?= bin/opticore
TEST_DSN ?= postgres://postgres:pgdocker1337@localhost:5432/opticore_test?sslmode=disable

.PHONY: help dev serve build migrate test itest fmt vet lint tidy clean db-test

help: ## list targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  %-10s %s\n", $$1, $$2}'

dev: ## run the daemon with hot reload (air)
	@command -v air >/dev/null || { echo "install: go install github.com/air-verse/air@latest"; exit 1; }
	set -a; . ./$(ENV_FILE); set +a; air

serve: ## run the daemon once
	set -a; . ./$(ENV_FILE); set +a; go run ./cmd/opticore serve

build: ## build the binary
	go build -o $(BIN) ./cmd/opticore

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
