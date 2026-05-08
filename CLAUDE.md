# hugen — Claude Operating Notes

## What hugen is

Native AI-agent runtime for the Hugr Data Mesh platform. One Go binary
that hosts the agent loop, tool dispatch, prompt assembly, skill
loading, and per-session lifecycle. Two run modes:

- **local** — embeds Hugr in-process via DuckDB; runs as a
  single-tenant analyst on the user's host.
- **hub** — connects to a remote Hugr deployment for catalog,
  memory, and data queries.

The agent treats Hugr as the LLM backend, the data exploration layer,
and the persistent memory store — all through GraphQL + MCP. We build
inside a Hugr ecosystem; constraints from that environment are
authoritative (see `.specify/memory/constitution.md` and
`assets/constitution/agent.md`).

## Where we are

We are mid-execution of **design 001 — Hugr Agent Runtime**
(`design/001-agent-runtime/design.md`).

**Architecture-of-record**: `design/001-agent-runtime/architecture.md`
is the current-state snapshot — read this first before touching
session / extension / tool / skill internals. Phase docs remain
authoritative as historical record of *why* each piece ended up
where it did, but `architecture.md` wins on questions of *how
things actually work today and where to plug in*.

Phase plan:

| Phase | Status |
|-------|--------|
| 1. Native core + ModelRouter + console UI | shipped |
| 2. HTTP/SSE + webui + ADK eviction | shipped |
| 3. Action layer (skills + tools + 3-tier permissions + bash/hugr-mcp) | shipped |
| 3.5. Analyst toolkit (duckdb-mcp + python-mcp + analyst skills) | shipped |
| **4. Sub-agents + plan + whiteboard + event-driven session loop** | **spec drafted v2** (`phase-4-spec.md`, ready for `/speckit.specify`); architecture decisions in `phase-4-architecture.md` |
| 4.1a. Extract `pkg/runtime` + dissolve `SystemProvider` (tools onto domain `ToolProvider`s; absorbs ex-4.3) | shipped (`33a0bc3`) |
| 4.1b. Observational scenario harness (port `../agent/tests/scenarios/` pattern; live LLM + real Hugr; ~8 scenarios v1) | shipped (`109f6b9`); 7 of 9 scenarios validated on gemini-pro + gemma4-26b, claude-sonnet canary green; `full_analyst_workflow` deferred → 4.2 |
| 4.1c. Subagent-as-adapter: parent observes child's outbox; eliminates child→parent.Submit cross-session shortcut. Surfaced by 4.1b harness (every sub-agent hung mid-flight). | shipped (`109f6b9`) — pump + retry + per-skill intent + plan envelope migration all in same merge |
| 4.2. Analyst mega-skill + role sub-agents + `skill_builder` + community-skill enablement (tri-state `allowed-tools`, `system:tool_catalog`) **+ task-complexity routing** (auto-classify when a task warrants a sub-agent vs. inline tool calls vs. just answering — gates `spawn_subagent` so root doesn't fan out trivial requests) | spec drafted v0 (`phase-4.2-spec.md`); follows 4.1c. Scope expanded 2026-05-08 to absorb the routing question that came out of 4.1b harness runs (root sometimes spawns when it shouldn't, sometimes does data work itself when it should delegate). |
| ~~4.3~~ | **cancelled 2026-05-08** — historical scope was Manager-as-ToolProvider generalisation; superseded by 4.1a (`SystemProvider` already dissolved) and the per-domain ToolProvider pattern that landed with it. |
| 5. Compactor + HITL: approvals + clarifications (compactor first within the phase; replaces the phase-3 `defaultHistoryWindow=50` stop-gap) | open |
| 6. Cron + scheduler | open |
| 7. Memory pipeline + LLM Wiki (short + long-term) | open |
| 8. Artifacts | open |
| 9. A2A adapter | open (defer until needed) |
| 10. Multi-party Workspaces (human + agent) | open — external interaction surface; lands after A2A so participant model is shaken out |
| Backlog. PeerGroup mesh / pipeline | deferred — whiteboard (phase 4) covers broadcast; mesh / pipeline re-introduce when a real workload demands it |

Goal: finish design-001 cleanly, then move to **hub integration**
(container packaging, deployment story, hub-spawned mode). Hub work is
explicitly deferred until design-001 is complete — `phase-3.5-spec.md
§Out of scope` and `design.md §16.8`.

## Active focus — phase 4.2

Phases 4 / 4.1a / 4.1b-pre / 4.1b / 4.1c all shipped to `main`
(latest merge `109f6b9`). The scenario harness lives in
`tests/scenarios/`; subagent pump + retry + per-skill intent +
plan→ExtensionFrame migration all landed with phase 4.1c. 4.3 is
**cancelled** (its scope was Manager-as-ToolProvider, which 4.1a
already absorbed).

Next phase is **4.2 — analyst skill + skill_builder + community
enablement + task-complexity routing**. The routing piece
(automatically deciding "spawn a sub-agent vs. answer inline vs.
do the tool call directly") was added on 2026-05-08 after 4.1b
runs showed every LLM family makes routing mistakes, but each in
a different way (Claude over-spawns trivial requests, gemma
sometimes under-delegates and tries to do data work itself).

Spec: `design/001-agent-runtime/phase-4.2-spec.md` (v0 draft).
Needs revision before implementation to spell out:

1. The classifier signal — system-prompt nudges only, or a
   dedicated `task_classify` tool the LLM calls before deciding,
   or a heuristic gate that fires before `spawn_subagent` is
   even visible. Open question.
2. The skill / role catalogue — what shape `analyst-mega-skill`
   takes, how role sub-agents declare their cost class
   (`Intent` already wired, but role description needs richer
   "when to use me" text).
3. `skill_builder` — author tooling for users to create their
   own skills + community publication path.

Treat 4.2 as an open discussion until the spec is revised — the
harness is the regression net, but the design isn't locked.

## Project structure

```text
cmd/
├── hugen/                # main binary — runtime bootstrap, console + webui adapters
└── hugen-skill-validate/ # CLI: validate a SKILL.md manifest
mcp/
├── bash-mcp/             # in-tree shell + filesystem MCP (per_session)
├── hugr-query/           # in-tree Hugr GraphQL → file output (per_agent)
└── python-mcp/           # in-tree Python execution + lazy per-session venv (per_agent)
pkg/
├── adapter/{console,http,webui}  # transport adapters
├── auth/{perm,sources,template}  # 3-tier permission stack + auth.Service loopback
├── config/                       # YAML schema + StaticService
├── identity/{local,hub}          # who-am-I providers
├── model/, models/, protocol/    # LLM routing + Frame protocol
├── session/                      # Session, Manager, Resources, Workspace, RuntimeStore
├── skill/                        # Manifest parser + SkillManager + stores (system/local/community/inline/hub)
├── store/local/                  # embedded DuckDB persistence
└── tool/                         # ToolManager + MCPProvider + SystemProvider + Policies
assets/
├── constitution/agent.md         # universal agent constitution (rendered into prompt)
├── python/requirements.txt       # bundled analyst venv package list
└── skills/                       # bundled skills: _system, hugr-data, duckdb-data, duckdb-docs, python-runner
vendor/
└── mcp-server-motherduck/        # vendored MotherDuck DuckDB MCP (git submodule, MIT)
design/001-agent-runtime/         # design + per-phase specs (gitignored after promotion)
specs/<NNN-feature-name>/         # per-feature speckit artefacts (gitignored)
.specify/memory/constitution.md   # Go code constitution (load-bearing rules)
```

## How I should behave (Claude operating mode)

I am logical, consistent, and grounded in the actual codebase. I do
not invent APIs, package layouts, or architectural decisions; I read
code first, propose second, edit third. When the user asks something
I do not know, I check (`grep`, `Read`, `gh`, build/test) before
answering. I am allowed to speculate — but every speculation must end
with "let me verify" or "this is a guess, please correct me", and I
evaluate every proposal against what already exists in the tree.

I am brief. End-of-turn summaries are one or two sentences. Updates
mid-task are one short line. I do not narrate internal deliberation.

Russian / English: I match the user's language.

## Flexibility with deliberation

We are **flexible on goals and architecture**: design-001 phases can
be re-ordered, scopes can be moved, providers can be added or
collapsed, package boundaries can shift. But every such change is a
deliberate decision, not a drift:

1. **Surface the question.** When I or the user notice the current
   plan no longer fits, I name it explicitly.
2. **Discuss before editing.** Architecture / goal pivots are
   discussed before code lands — even when the patch is small.
3. **Record and justify.** The decision goes into the relevant
   `design/001-agent-runtime/*.md` (or a new ADR-style note alongside
   it) with the *why*, the alternative considered, and what it
   supersedes. Implementation memos go into the per-phase spec's
   "Implementation update" footer (the pattern phase 3.5 used after
   `2026-05-01`).
4. **No silent rewrites.** Refactoring code that touches a documented
   contract requires updating the contract in the same PR or in an
   immediately-visible follow-up.

Code-level constitution (Go conventions, package layering, ADK
quarantine, append-only persistence) lives in
`.specify/memory/constitution.md` and is **non-negotiable** at PR
review. Goals and architecture are negotiable but only via the flow
above.

## Build / test cheatsheet

```sh
make build                       # bin/{hugen,bash-mcp,hugr-query,python-mcp}
make test                        # go test -race -tags=duckdb_arrow ./...
make submodule-check             # gate against vendored MCP drift
make python-mcp-template         # one-time analyst venv build (after make build)
go vet -tags=duckdb_arrow ./...  # static checks
```

Constitution gates (every PR): clean build with `-tags=duckdb_arrow`,
green tests, `go vet` clean, no new ADK imports below `pkg/models`,
new public function/method comes with happy-path + edge-case test.

## Speckit flow

Per-feature work goes through `/speckit.specify` → `/speckit.plan` →
`/speckit.tasks` under `specs/<NNN-feature-name>/`. The active feature
slot is `.specify/feature.json`. Outputs are gitignored by design
(per-developer) and only get reflected in `design/` when promoted.

<!-- MANUAL ADDITIONS START -->
<!-- MANUAL ADDITIONS END -->

## Active Technologies
- Go 1.23.x (project uses generics, `slices`, `maps`, (005-phase-4-agent-runtime)
- DuckDB local store via `pkg/store/local`; append-only on (005-phase-4-agent-runtime)

## Recent Changes
- 005-phase-4-agent-runtime: Added Go 1.23.x (project uses generics, `slices`, `maps`,
