GOLANGCI_LINT_VERSION=v1.27.0

LINTER=./bin/golangci-lint
LINTER_VERSION_FILE=./bin/.golangci-lint-version-$(GOLANGCI_LINT_VERSION)

SHELL=/bin/bash

build:
	go build ./...

test:
	go test -race -v ./...

$(LINTER_VERSION_FILE):
	rm -f $(LINTER)
	curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | bash -s $(GOLANGCI_LINT_VERSION)
	touch $(LINTER_VERSION_FILE)

lint: $(LINTER_VERSION_FILE)
	$(LINTER) run ./...

contract-tests: build
	@echo "Building contract test service..."
	@cd contract-tests && GOOS=linux go mod tidy && GOOS=linux go build
	@cd contract-tests && docker build --tag testservice .
	@docker run ldcircleci/sse-contract-tests:1 --url http://testservice:8000 --output-docker-script 1 \
		| bash

.PHONY: build lint test contract-tests
