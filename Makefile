BINARY_NAME        = origin-caster
CMD_DIR            = ./cmd/origin-caster
BUILD_DIR          = bin

CHROME_DEV_PROFILE ?= $(HOME)/.origin-caster-dev-profile
CHROME_DEBUG_PORT  ?= 9222
CHROME_BIN         ?= /Applications/Google Chrome.app/Contents/MacOS/Google Chrome

VERSION            ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT             ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME         ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

.PHONY: all build clean test test-coverage vet run list install chrome-dev help

all: build

## build: Compile the origin-caster binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

## test: Run all unit and integration tests (Go only; snippet JS tests run via test-js)
test:
	@echo "Running Go tests..."
	go test -v ./...

## test-js: Run the browser-snippet unit tests (web/cast.test.js, Node VM + fake DOM)
test-js:
	@echo "Running cast.js unit tests..."
	node --test web/cast.test.js

## test-coverage: Run tests with coverage report
test-coverage:
	@echo "Running tests with coverage..."
	go test -v -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

## vet: Run go vet linter
vet:
	@echo "Running go vet..."
	go vet ./...

## list: Scan and list all Chromecast devices on LAN
list: build
	./$(BUILD_DIR)/$(BINARY_NAME) -list

## run: Build and run
run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

## chrome-dev: Launch an isolated, separate Chrome instance with remote DevTools enabled (port 9222)
chrome-dev:
	@echo "Launching separate Chrome development instance on port $(CHROME_DEBUG_PORT)..."
	@mkdir -p "$(CHROME_DEV_PROFILE)"
	@"$(CHROME_BIN)" \
		--remote-debugging-port=$(CHROME_DEBUG_PORT) \
		--remote-debugging-address=127.0.0.1 \
		--disable-blink-features=AutomationControlled \
		--disable-features=BlockInsecurePrivateNetworkRequests,PrivateNetworkAccessRespectPreflightResults,PrivateNetworkAccessSendPreflights,LocalNetworkAccessChecks \
		--disable-web-security \
		--allow-running-insecure-content \
		--user-data-dir="$(CHROME_DEV_PROFILE)" \
		--no-first-run \
		--no-default-browser-check \
		> /dev/null 2>&1 &
	@echo "✓ Chrome Dev instance launched with isolated profile at $(CHROME_DEV_PROFILE)"
	@echo "✓ Remote DevTools listening on http://127.0.0.1:$(CHROME_DEBUG_PORT)"

## install: Install binary to GOPATH/bin
install:
	@echo "Installing $(BINARY_NAME)..."
	go install $(CMD_DIR)

## clean: Remove build artifacts and test coverage files
clean:
	@echo "Cleaning up..."
	rm -f $(BUILD_DIR)/$(BINARY_NAME) coverage.out

## help: Display available Makefile targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | awk -F ':' '{printf "  %-16s %s\n", $$1, $$2}'
