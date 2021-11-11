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
	@cd contract-tests && docker build --tag testserviceimage .
	@echo; echo "Starting contract test service..."
	@docker network create contract-tests-network 2>/dev/null || true
	@docker run --rm -d -p 8000:8000 --network contract-tests-network --name testservice testserviceimage
	@echo; echo "Running test harness"
	@docker run --rm -p 8111:8111 -it --network contract-tests-network --name testharness \
		ldcircleci/sse-contract-tests:1 --url http://testservice:8000 --host testharness \
		|| (echo; echo "Output from test service follows:"; docker logs testservice; docker stop testservice; exit 1)
	@docker stop testservice 2>/dev/null

.PHONY: build lint test contract-tests
