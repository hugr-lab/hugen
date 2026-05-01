.PHONY: build mcps bash-mcp hugr-query python-mcp python-mcp-template submodule-update submodule-check run run-console run-webui test vet lint check tidy clean

BINARY := bin/hugen
TAGS   := duckdb_arrow

# Debug-friendly CGO flags (DuckDB symbols visible in delve / stack traces).
CGO_DEBUG_FLAGS := -O1 -g

# Python venv template default location. Operators override via
# HUGEN_PYTHON_TEMPLATE env or the --out flag passed to python-mcp.
HUGEN_STATE          ?= $(HOME)/.hugen
HUGEN_PYTHON_TEMPLATE ?= $(HUGEN_STATE)/python-template/.venv

build: $(BINARY) mcps

$(BINARY):
	go build -tags=$(TAGS) -o $(BINARY) ./cmd/hugen

# MCP companion binaries spawned by the runtime over stdio.
mcps: bash-mcp hugr-query python-mcp

bash-mcp:
	go build -o bin/bash-mcp ./mcp/bash-mcp

hugr-query:
	go build -tags=$(TAGS) -o bin/hugr-query ./mcp/hugr-query

# Phase 3.5 — analyst toolkit binary. Owns the per-session Python
# venv lifecycle and the python.run_code / python.run_script tools.
# See mcp/python-mcp/doc.go for the full surface.
python-mcp:
	go build -o bin/python-mcp ./mcp/python-mcp

# Phase 3.5 — one-off operator command: build the relocatable venv
# template the runtime copies per session. Idempotent: re-runs return
# fast when the stamp file is up to date vs the requirements list.
# Override the destination via --out or HUGEN_PYTHON_TEMPLATE env.
# Requires uv >= 0.4.0 + Python >= 3.10 on PATH (see README §Prerequisites).
python-mcp-template: python-mcp
	./bin/python-mcp --create-template ./assets/python/requirements.txt --out $(HUGEN_PYTHON_TEMPLATE)

# Phase 3.5 — git submodule housekeeping. submodule-update is the
# operator-friendly initial-clone command; submodule-check is the CI /
# pre-build gate that refuses to compile against a divergent vendored
# tree (per spec FR-032).
submodule-update:
	git submodule update --init --recursive

submodule-check:
	@status=$$(git submodule status --recursive); \
	if echo "$$status" | grep -E '^[-+U]' >/dev/null; then \
		echo "FAIL: vendored submodule diverges from pinned SHA:"; \
		echo "$$status" | grep -E '^[-+U]'; \
		echo "Run 'make submodule-update' to restore, or update the pin via a deliberate commit."; \
		exit 1; \
	else \
		echo "OK: submodules pinned"; \
	fi

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

check: submodule-check vet build test
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
