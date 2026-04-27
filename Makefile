.PHONY: build test vet lint check tidy clean

TAGS := duckdb_arrow

# Debug-friendly CGO flags (DuckDB symbols visible in delve / stack traces).
CGO_DEBUG_FLAGS := -O1 -g

build:
	go build -tags=$(TAGS) ./...

test:
	CGO_CFLAGS="$(CGO_DEBUG_FLAGS)" go test -tags=$(TAGS) -race -count=1 ./...

vet:
	go vet -tags=$(TAGS) ./...

lint:
	golangci-lint run --build-tags=$(TAGS) ./...

check: vet test

tidy:
	go mod tidy

clean:
	go clean -cache -testcache
