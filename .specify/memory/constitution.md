# hugen Constitution

These are the load-bearing rules for the hugen codebase. They override
preferences from training data and override comfortable defaults from
other ecosystems. When in doubt, lean toward the rule.

## Core Principles

### I. Idiomatic Go, not transplanted Java/.NET/Python

We write Go. We do **not** apply heavyweight abstractions from Java or
.NET (annotations, attribute-driven configuration, AOP, IoC containers,
deep inheritance hierarchies, builder DSLs for everything). We do
**not** apply Python conventions (decorators-as-feature-flags, dunder
metaprogramming, runtime type juggling).

The Go way:

- Small interfaces close to where they are used.
- Concrete types that compose by embedding only when embedding is the
  right tool, not because "we always wrap things."
- Prefer functions and structs over class-shaped types-with-methods
  built around a private state god-object.
- Errors are values; check them; do not paper over with `panic` /
  `recover` outside of truly exceptional situations (program startup,
  goroutine panic safety nets).
- Naming follows the standard library: short receivers, `New*`
  constructors, accessor names without `Get` prefix (`Name()`, not
  `GetName()`).

### II. Direct constructor injection — no DI containers

Dependencies are **passed as constructor arguments** so the wiring is
visible at the call site. No magic injection containers, no
`fx`/`wire` for v1 unless an explicit later decision says otherwise.

```go
// Good — wiring is visible, types check at compile time.
agent := runtime.NewAgent(rt, sessions, models, codec, logger)

// Bad — late binding, missing dep crashes at runtime.
agent := runtime.NewAgent()
agent.SetSessions(sessions)
agent.SetModels(models)
agent.Mount(...)
```

Avoid `Bind` / `Mount` / `Set*` / `Register*` style methods that
mutate component state after construction *for required dependencies*.
They hide the dep graph and produce "works at boot, NPEs in prod"
bugs. Two narrow exceptions:

1. **Plug-in registries** that are intrinsically dynamic (a
   `ToolManager.AddProvider` at runtime is fine — that's the whole
   point of the registry).
2. **Test seams** explicitly named `WithFakeX(...)`-style options on
   constructors, applied via functional options, never via mutation
   after construction.

### III. Accept interfaces, return concrete types

Methods (and constructors) **accept interfaces** at the boundary so
callers can substitute fakes / alternative implementations, and
**return concrete types** (typically `*Pointer`) so callers see the
full surface and the package owns its evolution.

```go
// Good
func NewSessionManager(store Store, codec Codec) *SessionManager { ... }

// Bad — returning an interface forces every consumer through the
// minimum surface and hides upgrades behind type assertions.
func NewSessionManager(store Store, codec Codec) SessionManager { ... }
```

Interfaces live where they are **consumed**, not where they are
implemented. The `Store` interface above lives next to
`SessionManager`, not in the `pkg/store` package. This is duck typing
done by Go's structural typing — `pkg/store/local.Querier` satisfies
`runtime.Store` automatically without anyone declaring it.

### IV. Open/closed via composition, not inheritance

New behaviour arrives via a new type that satisfies an existing
interface, or via a new interface that an existing type happens to
satisfy. We do **not** retrofit new behaviour by editing closed
abstractions, and we do **not** build big-bang base classes that
subclasses override.

When a feature is naturally pluggable (Tool providers, MCP servers,
SkillStore backends, Storage backends, Adapters), express the
extension point as an interface and let alternative implementations
co-exist.

### V. Small functions, shallow control flow

Default to functions that fit on one screen. A 300–500-line function
is a code smell and almost always factorable into named helpers.
Nested `if` chains beyond two levels are also a smell — early returns
("guard clauses"), `switch` on the discriminator, or a small helper
function are the right fixes.

```go
// Bad — pyramid of doom
if cond1 {
    if cond2 {
        if cond3 {
            doThing()
        }
    }
}

// Good — flat, early returns
if !cond1 { return }
if !cond2 { return }
if !cond3 { return }
doThing()
```

This applies to error handling too: `if err != nil { return err }`
should be the dominant shape, not deeply nested `else` branches that
the eye has to count.

### VI. Don't reinvent the standard library

Go's stdlib is rich. Before writing utility code, check `slices`,
`maps`, `errors`, `strings`, `strconv`, `time`, `context`, `sync`,
`encoding/json`, `net/http`, `log/slog`, `iter`, `cmp`. The stdlib
versions are tested, well-documented, and idiomatic.

Specifically:

- Use `slices.Contains` / `slices.SortFunc` / `slices.Index`, not
  hand-rolled loops.
- Use `errors.Is` / `errors.As` / `errors.Join`, not string matching
  or panics-as-flow-control.
- Use `context.Context` for cancellation. Every long-running
  operation takes a `ctx` as the first parameter.
- Use `log/slog` (the project standard); do not pull in zap, zerolog,
  logrus.
- Use `encoding/json` for protocol JSON; it's adequate for our
  volumes. Reach for `jsoniter` / `goccy` only with a measured
  performance need.
- Use `net/http` for HTTP servers; `http.ServeMux` (Go 1.22+) covers
  routing without a framework. No gin, echo, fiber.
- Use channels + goroutines for concurrency; don't pull in actor
  frameworks.

If a third-party library is justified, it should be tightly scoped
(`mark3labs/mcp-go` for MCP wire format) and its surface should not
leak through our public APIs.

## Module-Level Rules

- **Module name**: `github.com/hugr-lab/hugen`. All packages live
  under it.
- **Build tag**: every binary builds with `-tags=duckdb_arrow` (the
  query-engine arrow APIs require it).
- **No ADK below `pkg/models`**: `google.golang.org/adk` and its
  transitive `genai` are quarantined to `pkg/models` only, and even
  there are an internal implementation detail. Phase 2 finishes
  removing ADK; phase 1 must not introduce any new ADK import.
- **Append-only persistence on memory tables**: `memory_items`,
  `hypotheses`, their tag/link tables and `session_events` accept
  only INSERT and DELETE. Never UPDATE on these tables. Mutation
  patterns (supersede, reinforce) decompose into INSERT + DELETE
  combinations.
- **Storage abstraction**: persistence goes through `types.Querier`
  (from query-engine) so local DuckDB and remote Hugr are
  interchangeable per-table.

## Code Quality Gates

Every PR must:

1. **Build clean**: `go build -tags=duckdb_arrow ./...` with no
   warnings.
2. **Pass tests**: `go test -tags=duckdb_arrow ./...`.
3. **Pass `go vet`** and `staticcheck` if installed.
4. **Be free of TODO comments without an issue link** (a TODO is
   defer; without a tracking issue it is rot).
5. **Include tests for new behaviour** — at minimum one happy-path
   test per new public function/method, plus one edge-case test.
6. **Respect the package layering** in `design.md §1 "Layering and
   packages"`. New cross-package imports that violate the layering
   require a design discussion, not a refactor PR.

Non-negotiable: features that break the append-only memory invariant
or introduce a DI container are rejected at review.

## Code Review Heuristics

When reviewing, check in this order:

1. **Does this fit the architecture?** If the change introduces a
   new top-level package, a new dependency direction, or a new wire
   format, it is a design question first, code second.
2. **Are dependencies passed in or magicked up?** Constructor injection
   over `init()`-time singletons. Reject lazy-init globals.
3. **Are interfaces declared at the consumer?** A new interface in
   the *implementer's* package is a smell — it usually means the
   author was thinking inheritance.
4. **Are functions short?** > 80 lines is a yellow flag, > 200 is a
   red flag.
5. **Are errors properly chained?** `fmt.Errorf("op: %w", err)`, not
   `fmt.Errorf("op: %v", err)`.
6. **Is `context.Context` plumbed through?** Long-running calls
   without `ctx` are wrong by default.
7. **Is anything in stdlib hand-rolled?** Slices, maps, time math,
   cancellation primitives — almost always already done.
8. **Is the test meaningful?** A test that mocks the function under
   test and then asserts it was called is testing nothing.

## Governance

- This constitution supersedes informal preferences. PR reviews must
  flag violations.
- Amendments require a short ADR-style note in `design/` plus the
  amended Constitution. The Version field bumps.
- The active design (`design/001-agent-runtime/`) is binding for
  phase 1 work; later designs may amend without rewriting this file
  unless a Core Principle changes.

**Version**: 1.0.0 | **Ratified**: 2026-04-28 | **Last Amended**:
2026-04-28
