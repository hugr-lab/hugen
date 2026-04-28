.PHONY: build run run-console run-webui test vet lint check tidy clean

BINARY := bin/hugen
TAGS   := duckdb_arrow

# Debug-friendly CGO flags (DuckDB symbols visible in delve / stack traces).
CGO_DEBUG_FLAGS := -O1 -g

build:
	go build -tags=$(TAGS) -o $(BINARY) ./cmd/hugen

run:
	go run -tags=$(TAGS) ./cmd/hugen

run-console:
	go run -tags=$(TAGS) ./cmd/hugen console

run-webui:
	go run -tags=$(TAGS) ./cmd/hugen webui

test:
	CGO_CFLAGS="$(CGO_DEBUG_FLAGS)" go test -tags=$(TAGS) -race -count=1 ./...

vet:
	go vet -tags=$(TAGS) ./...

lint:
	golangci-lint run --build-tags=$(TAGS) ./...

check: vet build test
	@echo "verifying ADK is quarantined to pkg/models..."
	@if go list -tags=$(TAGS) -deps ./pkg/protocol ./pkg/model ./pkg/runtime ./pkg/adapter/... 2>/dev/null | grep -E '^google\.golang\.org/(adk|genai)' | sort -u | grep .; then \
		echo "FAIL: ADK or genai imported below pkg/models (excluding cmd/hugen which legitimately uses pkg/models)"; exit 1; \
	else \
		echo "OK: no ADK below pkg/models"; \
	fi

tidy:
	go mod tidy

clean:
	go clean -cache -testcache
