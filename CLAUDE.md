# hugen Development Guidelines

Auto-generated from all feature plans. Last updated: 2026-04-28

## Active Technologies

- Go 1.26.1 (from `go.mod`) (001-agent-runtime-phase-1, 002-agent-runtime-phase-2)
- DuckDB via `pkg/store/local` (consumed through `pkg/runtime.RuntimeStore`); phase 2 reconnection replay queries `session_events` by `seq` ascending — no schema migration. (002-agent-runtime-phase-2)
- `net/http` + `embed.FS` for `pkg/adapter/http` (JSON+SSE) and `pkg/adapter/webui` (static UI). No third-party router, no JS bundler. (002-agent-runtime-phase-2)

## Project Structure

```text
src/
tests/
```

## Commands

# Add commands for Go 1.26.1 (from `go.mod`)

## Code Style

Go 1.26.1 (from `go.mod`): Follow standard conventions

## Recent Changes

- 002-agent-runtime-phase-2: HTTP/SSE adapter, web UI, ADK eviction; `cmd/hugen/runtime.go` (RuntimeCore) splits bootstrap from adapter wiring.
- 001-agent-runtime-phase-1: Native agent core, console adapter, ModelRouter, Frame protocol, RuntimeStore.

<!-- MANUAL ADDITIONS START -->
<!-- MANUAL ADDITIONS END -->
