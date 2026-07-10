# hugen — remote-mode agent container (Phase 8 / design-008 M3).
#
# Standalone build: the image compiles hugen + its in-tree MCP binaries from
# go.mod pins (NOT go.work — that's a dev-only overlay). The remote-agent fixes
# live in query-engine; bump the go.mod query-engine version to the release that
# carries them before building a production image.
#
# Run: hub-service (HB4/M4) spawns this injecting ONLY the remote-mode env
# (spec-agent-orchestration §3) — everything else is baked below:
#   HUGR_URL        hugr BASE url as seen from the agent network (NO /ipc —
#                   hugen appends it); HUGR_ISSUER (user-token issuer — boot-fatal
#                   if missing); HUGR_ACCESS_TOKEN (fresh one-shot bootstrap
#                   secret); HUGR_TOKEN_URL; optional HUGEN_LOG_LEVEL.
# The persistent /data mount (holds the agent JWT for restart-survival) is bound
# at spawn. The agent config (models / skills / tool_providers) comes from hub
# agent_info, not a local config.yaml. HUGEN_API_ALLOW_OPEN must never be set.

# ─────────────────────────────────────────────────────────────────────────────
# Stage 1 — build the Go binaries (CGO + DuckDB arrow).
# ─────────────────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS build

# CGO toolchain for the DuckDB driver (hugen + hugr-query compile the DuckDB
# C++ amalgamation — slow + memory-hungry, expected).
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# GOWORK=off — the standalone image builds from go.mod pins, not the dev
# go.work overlay. GOFLAGS=-mod=mod — the git submodule at ./vendor/
# (mcp-server-motherduck) makes go auto-enter vendor mode, which then fails
# "inconsistent vendoring" (no vendor/modules.txt); -mod=mod forces module mode.
ENV CGO_ENABLED=1 \
    GOWORK=off \
    GOFLAGS=-mod=mod

WORKDIR /src

# Warm the module cache first for layer reuse.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# Source (the vendored MotherDuck MCP submodule must be present in the context —
# it ships at runtime, but is not a Go build input).
COPY . .

# hugen + hugr-query need the duckdb_arrow tag; bash-mcp + python-mcp are plain.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -tags=duckdb_arrow -o /out/bin/hugen      ./cmd/hugen     && \
    go build -tags=duckdb_arrow -o /out/bin/hugr-query ./mcp/hugr-query && \
    go build                    -o /out/bin/bash-mcp   ./mcp/bash-mcp   && \
    go build                    -o /out/bin/python-mcp ./mcp/python-mcp

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2 — runtime.
# ─────────────────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim AS runtime

# TLS roots (hub / hugr over https) + git (uvx may fetch). No system Python:
# uv manages its own interpreter (used by python-mcp's venv template + the
# duckdb-mcp `uvx` provider).
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# uv / uvx — the venv builder for python-mcp and the runner for duckdb-mcp.
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /usr/local/bin/

# uv-managed Python + caches live in shared, writable locations so the venv
# template built here is reusable at runtime (and by a non-root user later).
ENV UV_PYTHON_INSTALL_DIR=/opt/uv/python \
    UV_CACHE_DIR=/opt/uv/cache \
    UV_PYTHON=3.12
RUN uv python install 3.12

WORKDIR /app

# Binaries + the vendored MotherDuck MCP + the venv requirements. Assets
# (skills / prompts / constitution) are embedded in the hugen binary — not
# copied. HUGEN_VENDOR_DIR defaults to <cwd>/vendor = /app/vendor, which the
# duckdb-mcp tool_provider references as ${HUGEN_VENDOR_DIR}/mcp-server-motherduck.
COPY --from=build /out/bin/ /app/bin/
COPY vendor/mcp-server-motherduck /app/vendor/mcp-server-motherduck
COPY assets/python/requirements.txt /app/assets/python/requirements.txt

# Bake the relocatable Python venv template python-mcp copies per session
# (equivalent to `make python-mcp-template`), so first boot is fast + offline.
# Some analyst deps ship no arm64 wheel (e.g. multimark, a great-tables dep) and
# compile from sdist → a C toolchain is needed for the build. Install it and
# purge it in the SAME layer so the slim runtime keeps no compiler and the
# committed layer carries only the built venv.
RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && /app/bin/python-mcp --create-template /app/assets/python/requirements.txt \
        --out /app/python-template/.venv \
    && apt-get purge -y build-essential && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

# Runtime path contract. /data is the mount root HB4 binds:
#   state     — PERSISTENT: agent JWT (hugr-token.json) => restart-survival,
#               plus materialised skills/ and artifacts/.
#   workspace — per-session scratch.
#   shared    — bash-mcp SHARED_DIR.
ENV HUGEN_STATE=/data/state \
    HUGEN_WORKSPACE_DIR=/data/workspace \
    HUGEN_SHARED_ROOT=/data/shared \
    HUGEN_ARTIFACTS_DIR=/data/artifacts \
    HUGEN_PYTHON_TEMPLATE=/app/python-template/.venv \
    HUGEN_VENDOR_DIR=/app/vendor \
    HUGEN_PORT=10000 \
    HUGEN_API_PORT=10200
RUN mkdir -p /data/state /data/workspace /data/shared /data/artifacts
VOLUME ["/data"]

EXPOSE 10200

# Liveness probe consumed by the M4 supervisor (spec-agent-orchestration §2/§4).
# start-period covers the slow remote boot: token-exchange backoff alone is
# 5+30+150s and /healthz is served only AFTER the full boot completes, so a
# shorter window would mark a healthy-but-still-booting agent unhealthy and
# trigger a recreate storm. Probe = `hugen healthcheck` (GET /healthz on
# HUGEN_API_PORT); no curl in the slim image.
HEALTHCHECK --interval=30s --timeout=5s --retries=5 --start-period=300s \
    CMD ["/app/bin/hugen", "healthcheck"]

# Remote mode is auto-selected by HUGR_URL + HUGR_ACCESS_TOKEN + HUGR_TOKEN_URL,
# which HB4 injects at spawn. NOTE: runs as root for M3 — non-root hardening
# (a dedicated uid owning /app + /data) is a follow-up.
ENTRYPOINT ["/app/bin/hugen"]
CMD ["serve"]
