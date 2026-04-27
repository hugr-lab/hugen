# hugen

Universal AI agent runtime for the [Hugr](https://hugr-lab.github.io) Data Mesh platform.

> **Status:** clean-slate rewrite. The previous experimental implementation lives in [`hugr-lab/agent`](https://github.com/hugr-lab/agent) and is being rebuilt here from first principles based on lessons learned.

## What it is

`hugen` is a Hugr-native agent runtime: it treats Hugr as the LLM backend, the data exploration layer, and the persistent memory store — all reached through GraphQL and MCP. The same binary runs in two modes:

- **Local / standalone** — embeds Hugr in-process and attaches a local DuckDB file as the runtime catalog. LLM and embedding providers are registered inside the embedded engine, so the agent runs without any external Hugr instance.
- **Hub** — connects to a remote Hugr deployment and uses its catalog for memory, models, and data queries.

## Goals of the rewrite

- A small, dependency-light core that keeps the agent loop, tool dispatch, prompt assembly, and skill loading in one explicit place.
- Universal agent shape: any specialization (analyst, data engineer, coordinator, …) is expressed as a skill package + configuration, not as a fork of the runtime.
- Stable contracts between the runtime, skills, and Hugr — so skills can be authored, versioned, and reloaded without rebuilding the agent.
- Spec-driven development via [spec-kit](https://github.com/github/spec-kit): everything non-trivial starts as a numbered design / spec / plan under `.specify/` before code lands.

## Required external services

The agent will keep two hard runtime dependencies (startup aborts if either is unreachable):

- **LLM completion endpoint** — OpenAI-compatible `/v1/chat/completions` (LM Studio, vLLM, Ollama, or a cloud provider).
- **Embedder endpoint** — OpenAI-compatible `/v1/embeddings`. Long-term memory and classified session events are vectorized; there is no keyword-only fallback.

Both can point at the same local server (e.g. LM Studio with an LLM and an embedding model loaded simultaneously), or at separate hosted providers.

## Repository layout (planned)

```text
.specify/        spec-kit config, templates, designs, specs
.claude/skills/  spec-kit skills (specify, plan, tasks, design, …)
cmd/             entry points
pkg/             public packages
internal/        runtime internals
skills/          skill packages (SKILL.md + references/ + mcp.yaml)
```

This repository is currently scaffold-only. Initial designs and specs land under `.specify/` first; runtime code follows.

## License

[MIT](./LICENSE)
