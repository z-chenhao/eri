SHELL := /bin/sh

GO_FILES := $(shell find . -type f -name '*.go' -not -path './vendor/*' -not -path './.git/*' -not -path './.eri/*' | sort)
VERSION ?= v0.0.0-dev

.PHONY: help build build-extensions run fmt fmt-check mod-check test coverage-check vet docs-check shell-check workflow-check ci-script-test release-dist check

help:
	@echo "Eri development commands"
	@echo "  make build       Build the single Eri binary"
	@echo "  make build-extensions  Build optional Google Workspace plugin and Auth Broker"
	@echo "  make run         Run the Eri daemon in the foreground"
	@echo "  make fmt         Format Go files"
	@echo "  make mod-check   Verify go.mod and go.sum are tidy"
	@echo "  make test        Run Go tests when go.mod exists"
	@echo "  make coverage-check  Enforce cross-package coverage floor"
	@echo "  make vet         Run go vet when go.mod exists"
	@echo "  make release-dist VERSION=vX.Y.Z"
	@echo "  make check       Run all repository checks"

build:
	@mkdir -p bin
	@go build -o bin/eri ./cmd/eri

build-extensions:
	@mkdir -p bin
	@go build -o bin/eri-google-workspace ./cmd/eri-google-workspace
	@go build -o bin/eri-google-auth-broker ./cmd/eri-google-auth-broker

run:
	@go run ./cmd/eri daemon

fmt:
	@if [ -n "$(GO_FILES)" ]; then \
		gofmt -w $(GO_FILES); \
	else \
		echo "fmt: no Go files; skipped"; \
	fi

fmt-check:
	@if [ -n "$(GO_FILES)" ]; then \
		unformatted="$$(gofmt -l $(GO_FILES))"; \
		if [ -n "$$unformatted" ]; then \
			echo "Go files need formatting:"; \
			echo "$$unformatted"; \
			exit 1; \
		fi; \
	else \
		echo "fmt-check: no Go files; skipped"; \
	fi

mod-check:
	@if [ -f go.mod ]; then \
		go mod tidy -diff; \
	else \
		echo "mod-check: go.mod not initialized; skipped"; \
	fi

test:
	@if [ -f go.mod ]; then \
		go test ./...; \
	else \
		echo "test: go.mod not initialized; skipped"; \
	fi

coverage-check:
	@./scripts/check-coverage.sh

vet:
	@if [ -f go.mod ]; then \
		go vet ./...; \
	else \
		echo "vet: go.mod not initialized; skipped"; \
	fi

docs-check:
	@./scripts/check-repo.sh

shell-check:
	@for script in scripts/*.sh; do sh -n "$$script"; done

workflow-check:
	@./scripts/check-workflows.sh

ci-script-test:
	@./scripts/check-pr-metadata_test.sh

release-dist:
	@./scripts/build-release.sh "$(VERSION)" dist

check: docs-check shell-check workflow-check ci-script-test fmt-check mod-check vet test
