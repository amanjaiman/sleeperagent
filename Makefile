# SleeperAgent build tasks.
BINARY      := sleeperagent
PKG         := ./cmd/sleeperagent
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: build test vet fmt fmtcheck check install clean dist

build: ## Build the binary for the host platform
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

install: ## go install the binary
	go install -ldflags "$(LDFLAGS)" $(PKG)

test: ## Run unit tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go files
	gofmt -w .

fmtcheck: ## Fail if any Go file is not gofmt-clean
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

check: fmtcheck vet test ## Run all checks (used by CI)

clean: ## Remove build artifacts
	rm -f $(BINARY) $(BINARY)-* *.test

# Cross-compiled release binaries (set VERSION=vX.Y.Z for a real release).
dist: ## Build linux/darwin/windows binaries into ./dist
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64   $(PKG)
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64   $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-amd64  $(PKG)
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64  $(PKG)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe $(PKG)
