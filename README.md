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

## Operator setup — analyst toolkit (phase 3.5)

The `duckdb-mcp` and `python-mcp` providers in `config.example.yaml` are
optional. To enable them, the operator installs three sets of host
prerequisites once and then builds the Python venv template once.

### 1. Host packages

| Package | Minimum | Why |
|---------|---------|-----|
| Python | 3.10+ | runs every `python-mcp` subprocess; the relocatable venv builder is invoked against this interpreter |
| [`uv`](https://docs.astral.sh/uv/) | 0.4.0+ | builds the relocatable venv (`uv venv --relocatable`) and resolves packages (`uv pip install`) |
| Cairo / Pango / gdk-pixbuf / libffi | system | runtime deps of `weasyprint` (HTML → PDF) |

Install commands (one-time):

```sh
# macOS
brew install python@3.12 uv cairo pango gdk-pixbuf libffi

# Debian / Ubuntu
apt-get install -y python3 uv libcairo2 libpango-1.0-0 \
  libpangoft2-1.0-0 libgdk-pixbuf-2.0-0 libffi8

# Fedora / RHEL
dnf install -y python3 uv cairo pango gdk-pixbuf2 libffi
```

### 2. Vendored MCP submodule

`duckdb-mcp` is the upstream MotherDuck MCP server pinned as a git
submodule under `vendor/mcp-server-motherduck/`:

```sh
git submodule update --init --recursive
```

`make submodule-check` refuses to build when the submodule SHA diverges
from the pinned tag; updates are operator-driven submodule pin bumps,
never in-tree patches.

### 3. Python venv template

`python-mcp` needs a relocatable virtualenv that every session's first
call lazily copies into its workspace. Build it once after `make`:

```sh
make python-mcp-template
```

This runs `./bin/python-mcp --create-template ./assets/python/requirements.txt`
and produces `${HUGEN_STATE}/python-template/.venv/.bootstrap-complete`.
The bundled requirements list contains `pandas`, `pyarrow`, `duckdb`,
`hugr-client`, `matplotlib`, `plotly`, `great_tables`, `folium`,
`weasyprint` (no version pins — `uv` resolves latest at build time).

Override the template location with `HUGEN_PYTHON_TEMPLATE=/abs/path`
or the `--out` flag when calling the binary directly.

### 4. Runtime-injected env

The runtime (not the operator) sets these env vars on every analyst
provider spawn:

| Var | Source | Used by |
|-----|--------|---------|
| `WORKSPACES_ROOT` | `HUGEN_WORKSPACE_DIR` | `python-mcp` (locates `<sid>/.venv`); `duckdb-mcp` cwd |
| `HUGR_TOKEN_URL` | `auth.Service` loopback | `python-mcp` (when `auth: hugr` is set on the provider) |
| `HUGR_ACCESS_TOKEN` | per-spawn bootstrap | same; the binary exchanges it for fresh JWTs |

Operators MUST NOT set these in the YAML. To run without Hugr, drop
`auth: hugr` from the `python-mcp` entry and the binary works in
"no Hugr" mode (Python scripts that import `hugr-client` will error at
runtime, but every other workflow keeps working).

### 5. Drop-in fallback

`duckdb-mcp` and `python-mcp` are independent. Drop either or both from
`tool_providers:` to fall back to the phase-3 baseline (bash-mcp + Hugr
+ system tools). Skills that grant tools from a missing provider are
flagged unavailable rather than rejected.

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
