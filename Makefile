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

TEMP_TEST_OUTPUT=/tmp/sse-contract-test-service.log

contract-tests:
	@cd contract-tests && go mod tidy && go build
	@./contract-tests/contract-tests >$(TEMP_TEST_OUTPUT) &
	@curl -s https://raw.githubusercontent.com/launchdarkly/sse-contract-tests/v0.0.3/downloader/run.sh \
      | VERSION=v0 PARAMS="-url http://localhost:8000 -stop-service-at-end" sh || \
      (echo "Tests failed; see $(TEMP_TEST_OUTPUT) for test service log"; exit 1)

.PHONY: build lint test contract-tests
