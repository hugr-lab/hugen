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
(`design/001-agent-runtime/design.md`). Phase plan:

| Phase | Status |
|-------|--------|
| 1. Native core + ModelRouter + console UI | shipped |
| 2. HTTP/SSE + webui + ADK eviction | shipped |
| 3. Action layer (skills + tools + 3-tier permissions + bash/hugr-mcp) | shipped |
| 3.5. Analyst toolkit (duckdb-mcp + python-mcp + analyst skills) | shipped |
| **4. Sub-agents + plan + whiteboard + event-driven session loop** | **spec drafted v2** (`phase-4-spec.md`, ready for `/speckit.specify`); architecture decisions in `phase-4-architecture.md` |
| 4.1a. Extract `pkg/runtime` + dissolve `SystemProvider` (tools onto domain `ToolProvider`s; absorbs ex-4.3) | spec drafted v1 (`phase-4.1a-spec.md`); follows 4, gates 4.1b |
| 4.1b. Observational scenario harness (port `../agent/tests/scenarios/` pattern; live LLM + real Hugr; ~8 scenarios v1) | spec drafted v1 (`phase-4.1b-spec.md`); follows 4.1a, gates 4.1c |
| 4.1c. Subagent-as-adapter: parent observes child's outbox; eliminates child→parent.Submit cross-session shortcut. Surfaced by 4.1b harness (every sub-agent hung mid-flight). | spec drafted v1 (`phase-4.1c-spec.md`); follows 4.1b, gates 4.2 |
| 4.2. Analyst mega-skill + role sub-agents + `skill_builder` + community-skill enablement (tri-state `allowed-tools`, `system:tool_catalog`) | spec drafted v0 (`phase-4.2-spec.md`); follows 4.1c |
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

## Active focus — phase 4.1c

Phases 4 / 4.1a / 4.1b-pre shipped to `main`
(`ba003c0` / `33a0bc3` / `8a2719e`). Phase 4.1b (scenario
harness) lives on branch `009-scenario-harness`; the harness
surfaced an architectural bug — every `Spawn`-ed sub-agent
hangs mid-flight because no consumer exists for child's
outbox. Active focus is **phase 4.1c** on branch
`010-fix-subagent-delivery` to fix it before the manual
multi-LLM run (4.1b §12 step 8) can complete and 4.2 starts.

**Document set for 4.1c work** (in order of authority):

1. **`design/001-agent-runtime/phase-4.1c-spec.md`** — the
   contract. §1 Why, §2 Two symmetric channels, §3 Pump,
   §4 Kind-level dispatch, §5 End-to-end flow, §6 Abnormal
   terminations, §7 What does NOT change, §8 Files, §9
   Implementation order, §10 Verification.
2. **`design/001-agent-runtime/phase-4.1b-spec.md`** — the
   harness; `step 8` (manual runs across 4 LLMs) is blocked
   on this fix.
3. **`pkg/protocol/frame.go:21-58`** — closed kind set the
   pump filters against. No protocol change in 4.1c.

**Key locked decisions for 4.1c**:

- Parent runs one **fire-and-forget pump goroutine** per child
  (`Session.consumeChildOutbox`). Started from `Spawn` after
  `child.Start(ctx)`. Lifecycle = channel close.
- Pump's switch is **kind-level only** — no payload spelunking,
  no extension-name knowledge. `AgentMessage{Final&&Consolidated}`
  → "result", `Error{!Recoverable}` → "terminal error",
  `SessionTerminated` → fallback projection. Everything else
  drains.
- **`SubagentResult` kind stays unchanged in `pkg/protocol/`**;
  only the producer moves from child to parent. Frame is
  constructed by parent's pump and Submit'd to parent's own
  inbox so `routeInbound` → `handleSubagentResult` →
  `wait_subagents` feed pipeline works unchanged.
- `emitSubagentResultToParent` is **deleted**.
  `requestClose` for subagents now self-Submits SessionClose.
  `subagentResultSent` atomic gate is gone — single producer
  (the pump) replaces the dual child-side producers.
- `handleExit` for subagents pushes its `SessionTerminated`
  frame onto the outbox (best-effort, recover-safe) so the pump
  observes the actual reason; root sessions skip this push.
- **Whiteboard / plan / all extensions are NOT touched** —
  `parentState.Submit` is a legitimate extension-level
  cross-session contract, not the child-runtime shortcut.
- 4.1c is **one PR** on branch `010-fix-subagent-delivery`.

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
