.PHONY: build build-go cli run clean test test-coverage lint fmt web dev-web dev-api

BINARY_NAME=vaults3
CLI_NAME=vaults3-cli
BUILD_DIR=.

# Build version injected into the binary (nearest git tag, or "dev" outside a repo).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS=-X main.version=$(VERSION)

# Build React frontend
web:
	cd web && npm ci && npm run build
	rm -rf internal/dashboard/dist
	cp -r web/dist internal/dashboard/dist

# Build Go binary (includes frontend)
build: web
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/vaults3
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(CLI_NAME) ./cmd/vaults3-cli

# Build Go only (skip frontend, for backend-only dev)
build-go:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/vaults3
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(CLI_NAME) ./cmd/vaults3-cli

# Build CLI only
cli:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(CLI_NAME) ./cmd/vaults3-cli

run: build
	./$(BINARY_NAME)

# Dev mode: Vite dev server with proxy to Go backend
dev-web:
	cd web && npm run dev

# Dev mode: Go backend only
dev-api: build-go
	./$(BINARY_NAME)

clean:
	rm -f $(BINARY_NAME) $(CLI_NAME)
	rm -rf data/ metadata/
	rm -rf internal/dashboard/dist
	rm -rf web/node_modules web/dist

test:
	go test ./...

test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	@echo "HTML report: go tool cover -html=coverage.out"

lint:
	@which golangci-lint > /dev/null 2>&1 || echo "Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"
	golangci-lint run ./...

fmt:
	go fmt ./...
