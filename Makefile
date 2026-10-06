build: ## Build the application
	go build -o output/mcpproxy ./mcpproxy

install: ## Install the application
	go install ./mcpproxy
	@echo "Installed to $(shell go env GOPATH)/bin/mcpproxy"

test: ## Run unit tests
	go test -v ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

fmt: ## Run format
	gofumpt -extra -w .
	golangci-lint fmt

lint: ## Run lint
	golangci-lint run

fix: ## Run fix
	golangci-lint run --fix

help: ## Display this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "%-20s %s\n", $$1, $$2}'

.PHONY: build install test test-race fmt lint fix help
