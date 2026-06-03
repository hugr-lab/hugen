// Package extension defines the contract for pluggable session
// extensions: discrete components (plan, whiteboard, skills,
// notepad, future plugins) the runtime composes onto every session
// at boot. An extension is a normal Go value; the capability
// interfaces below describe the pipelines the runtime hooks it
// into. An extension does not need to implement all of them — the
// runtime asserts each capability and wires it where it applies.
//
// Extensions are agent-level singletons constructed once during
// runtime boot (pkg/runtime). Per-session state lives in a
// [SessionState] handle the extension stores via SetValue at
// session creation; subsequent calls (tool dispatch, prompt
// rendering, frame routing) read it via Value.
//
// See pkg/protocol.ExtensionFrame for the envelope through which
// extension-defined events flow on the persistence layer and the
// transport.
package extension

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Extension is the marker interface every plug-in implements.
// Name returns a stable identifier (e.g. "plan", "whiteboard",
// "skill") used as the namespace key in [SessionState] and as
// the routing discriminator on [protocol.ExtensionFrame]. Names are
// case-sensitive and must be unique across the registered set; the
// runtime panics on a duplicate registration.
type Extension interface {
	Name() string
}

// StateInitializer extensions create their per-session state at
// session construction. Called once from session.NewSession after
// the shell is built and before the goroutine starts (so the
// handle is observable to the very first inbound frame). Idempotent
// is not required — InitState runs exactly once per session.
//
// Implementations typically allocate their projection struct and
// call state.SetValue(ext.Name(), handle); subsequent capability
// calls retrieve it via state.Value(ext.Name()).
type StateInitializer interface {
	InitState(ctx context.Context, state SessionState) error
}

// Recovery extensions rebuild their per-session state from the
// session's event log. Called lazily on the first inbound frame
// (materialise) — once per session lifetime. The state handle is
// already populated by InitState; Recovery mutates it by replaying
// matching events. Implementations filter the rows by
// EventType/Extension/Op as appropriate; the runtime hands over
// the full event slice without pre-filtering so a single pass over
// the log suffices for every Recovery extension.
//
// Returning an error logs a warning; recovery is best-effort and
// must not block session start.
type Recovery interface {
	Recover(ctx context.Context, state SessionState, events []store.EventRow) error
}

// Shutdowner extensions clean up agent-level resources at runtime
// shutdown — background goroutines, pooled file handles, network
// clients, anything that outlives an individual session. The
// runtime walks every Shutdowner-implementing extension in
// reverse registration order during graceful shutdown (after
// every active session has terminated, before pkg/runtime closes
// the local store).
//
// Distinct from [Closer]: Closer.CloseSession runs once per
// session at session-teardown; Shutdown runs once at process
// shutdown. Extensions that hold ONLY per-session state implement
// just Closer; extensions with agent-level state (mcp's
// per_agent providers managed via the runtime tool registry,
// future workers, …) implement Shutdown.
//
// Errors are logged but do not abort the runtime shutdown sweep —
// every extension gets a chance to drain regardless.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

// Closer extensions release per-session resources at session
// teardown. Called from session.teardown after lifecycle.Release
// and before the per-session ToolManager closes. Errors are logged
// but do not abort teardown — close paths must drain regardless.
//
// CloseSession (rather than Close) so an extension that also
// implements [tool.ToolProvider] can satisfy both interfaces
// without a name clash with ToolProvider.Close().
//
// Optional: extensions whose state is plain memory (plan,
// whiteboard projections) skip this; only extensions that hold
// goroutines, file handles, or external bindings implement it.
type Closer interface {
	CloseSession(ctx context.Context, state SessionState) error
}

// Advertiser extensions contribute a section to the system prompt
// the runtime feeds into the model on each turn. The returned
// string is concatenated into the prompt verbatim (with newline
// separation). An empty string skips the section. Sections are
// emitted in extension-registration order; v1 does not provide
// ordering primitives — order this matters to is documented at the
// registration site.
type Advertiser interface {
	AdvertiseSystemPrompt(ctx context.Context, state SessionState) string
}

// ToolFilter extensions narrow the per-session tool catalogue.
// Called by Session.Snapshot when the cached snapshot is rebuilt
// (cache invalidates on toolGen / policyGen / extension generation
// bumps). Multiple filters compose by intersection — the most
// restrictive wins. Implementations must be deterministic for a
// given (state, all) pair so the snapshot cache stays correct.
type ToolFilter interface {
	FilterTools(ctx context.Context, state SessionState, all []tool.Tool) []tool.Tool
}

// GenerationProvider is an optional capability extensions implement
// when their per-session state changes in a way that should
// invalidate the snapshot cache. Generation receives the calling
// session's SessionState so per-session counters (e.g. skill
// bindings) fit naturally; the runtime sums the value with other
// extensions' generations and folds the result into the cache key.
// Generation must be monotonically non-decreasing for the lifetime
// of (extension, session). Extensions whose state is bind-once at
// construction (notepad) skip this interface.
type GenerationProvider interface {
	Generation(state SessionState) int64
}

// ToolIterPolicy is the per-turn tool-loop policy an extension
// recommends for the calling session. The runtime composes
// recommendations across every [ToolPolicyAdvisor] extension by
// taking the largest non-zero SoftCap / HardCeiling and a logical
// OR over DisableStuckNudges (the loosest setting wins by intent
// — an explorer skill raising the budget shouldn't be undone by a
// utility skill keeping the default).
type ToolIterPolicy struct {
	// SoftCap is the per-turn tool-iteration cap. Zero means "no
	// recommendation"; the runtime falls back to its session-level
	// override / default.
	SoftCap int

	// HardCeiling is the per-turn hard ceiling at which the runtime
	// terminates with reason "hard_ceiling". Zero means "no
	// recommendation"; the runtime falls back to 2 × SoftCap (or
	// its default).
	HardCeiling int

	// DisableStuckNudges asks the runtime to silence the
	// stuck-detection nudges for this session. Multiple extensions
	// compose by OR — a single advisor disabling is enough.
	DisableStuckNudges bool
}

// ToolPolicyAdvisor extensions advise the per-turn tool-loop
// policy. Sampled once at the top of every user turn alongside
// resolveToolIterCap; results stay stable through the loop even
// if a tool call mutates extension state mid-turn.
//
// Skill ext is the canonical advisor today (loaded skills'
// metadata.hugen.{max_turns,max_turns_hard,stuck_detection}); a
// future plan / whiteboard ext could lift the cap for analyst
// flows.
type ToolPolicyAdvisor interface {
	AdviseToolPolicy(ctx context.Context, state SessionState) ToolIterPolicy
}

// SubagentValidation is the outcome a [SubagentDescriber]
// extension reports for a (skill, role) pair on spawn_subagent
// validation.
type SubagentValidation int

const (
	// SubagentUnknown — this advisor has no information about the
	// requested skill (e.g. the skill ext queried but the skill
	// catalog doesn't list it). Composition rule: if every advisor
	// returns Unknown, the runtime treats the spawn as
	// skill_not_found.
	SubagentUnknown SubagentValidation = iota

	// SubagentValid — skill exists; role is either omitted or
	// matches a declared subagent role.
	SubagentValid

	// SubagentSkillFoundRoleMissing — skill exists in this advisor's
	// catalog but the requested role is not declared on it.
	SubagentSkillFoundRoleMissing
)

// SubagentDescriber extensions validate spawn_subagent's
// (skill, role) tuple against their own knowledge of the skill
// catalog. The runtime composes results across every advisor:
// SubagentValid from any advisor wins; otherwise if any reports
// SubagentSkillFoundRoleMissing the spawn surfaces as
// role_not_found; otherwise (every advisor returned Unknown) the
// spawn surfaces as skill_not_found. A session with no advisor
// registered short-circuits to "no validation" — matching the
// pre-stage-4 behaviour when no SkillManager was wired.
//
// Skill ext is the canonical advisor today; future plan /
// whiteboard extensions could declare their own dispatch surfaces.
type SubagentDescriber interface {
	DescribeSubagent(ctx context.Context, state SessionState, skill, role string) (SubagentValidation, error)
}

// SubagentSpawnHint carries optional per-role spawn-time hints the
// runtime applies to a freshly-Spawn'ed child. Today only Intent —
// names the model-router intent the child resolves through (default
// | cheap | tool_calling | …). Empty intent leaves the parent's
// default in place.
//
// Future fields (max_turns override, system_prompt prefix, etc.)
// land here without breaking call-site shape.
type SubagentSpawnHint struct {
	Intent string
}

// SubagentSpawnHinter is a sibling capability of [SubagentDescriber]
// — extensions that own a (skill, role) record can report spawn-time
// configuration the runtime applies after Session.Spawn returns. The
// runtime calls every registered hinter; the first non-empty Intent
// wins (deterministic order: registration order). Returning a zero
// hint means "no opinion" and the next hinter / the runtime default
// applies.
//
// Skill ext is the canonical hinter today; future plan / whiteboard
// extensions could declare their own dispatch surfaces with their
// own preferred intents.
type SubagentSpawnHinter interface {
	SubagentSpawnHint(ctx context.Context, state SessionState, skill, role string) (SubagentSpawnHint, error)
}

// SubagentSpawnApplier is the side-effect counterpart of
// [SubagentSpawnHinter]: instead of returning data the runtime
// applies, the extension itself mutates the freshly-spawned child.
// Canonical use today is role-declared skill autoload — the skill
// extension reads `sub_agents[*].autoload_skills` from the
// dispatching skill's manifest and invokes the child's per-session
// loader BEFORE the child's first model turn.
//
// Run-once contract: the runtime calls every registered applier
// exactly once per spawn, AFTER Session.Spawn returns and AFTER
// the intent hint has been applied, but BEFORE the task
// UserMessage lands in the child's inbox. Errors are logged and
// skipped — a misbehaving applier must not block the spawn.
type SubagentSpawnApplier interface {
	ApplyOnSubagentSpawn(ctx context.Context, child SessionState, skill, role string) error
}

// MissionDispatcher extensions own a skill catalogue and can
// answer whether a given skill name is registered AND declares
// itself mission-eligible (metadata.hugen.mission.enabled:true).
// session:spawn_mission consults this interface to validate the
// caller's `skill` argument before spawning a child.
//
// The runtime calls every registered dispatcher; the first
// affirmative answer wins (deterministic registration order). A
// session with no dispatcher registered short-circuits to "skill
// accepted" — preserves the pre-γ behaviour for fixture tests
// that never wire a SkillManager. Phase 4.2.2 §6.
type MissionDispatcher interface {
	// MissionSkillExists reports whether `skill` is installed AND
	// declares metadata.hugen.mission.enabled:true. Empty `skill`
	// always returns (false, nil) — empty-argument resolution is
	// the caller's concern via deps.DefaultMissionSkill.
	MissionSkillExists(ctx context.Context, skill string) (bool, error)
}

// MissionStartBlock carries the rendered on_start instructions a
// MissionStartLookup returns at mission boot. All fields are
// post-template strings; consumers (runtime) apply them without
// further templating. Phase 4.2.2 §7.
type MissionStartBlock struct {
	// PlanText is the rendered plan body to seed via PlanSystemWriter.
	// Empty skips the plan step.
	PlanText string

	// PlanCurrentStep is the focus pointer accompanying PlanText.
	PlanCurrentStep string

	// WhiteboardInit, when true, triggers a WhiteboardSystemWriter
	// call before the mission's first turn. Idempotent on already-
	// active boards.
	WhiteboardInit bool

	// FirstMessageOverride replaces the bare `goal` string as the
	// mission's first user-role message. Empty defers to the
	// caller-provided goal.
	FirstMessageOverride string
}

// MissionStartLookup extensions resolve the on_start block for a
// dispatching skill at spawn time, running any per-skill template
// substitutions before returning. Returns (nil, nil) when the
// skill is not mission-enabled or declares no on_start. Phase
// 4.2.2 §7.
//
// Skill ext is the canonical implementor (it owns the manifest).
type MissionStartLookup interface {
	ResolveMissionStart(ctx context.Context, skill, goal string, inputs any) (*MissionStartBlock, error)
}

// PlanSystemWriter extensions expose a direct in-process plan-set
// path bypassing the ToolManager — the runtime uses it to seed a
// mission's plan from on_mission_start. Not callable from LLM
// tool dispatch; the runtime is the sole authorised caller.
// Phase 4.2.2 §7.
type PlanSystemWriter interface {
	SystemSet(ctx context.Context, state SessionState, text, currentStep string) error
}

// CloseTurnBlock carries the rendered on_close configuration the
// runtime applies before tearing a session down. Phase 4.2.3 ε.
// Returned by [CloseTurnLookup.ResolveCloseTurn] when at least
// one loaded skill opted into a close turn for the calling
// session's role/tier.
//
// The runtime uses these fields to fire one constrained model
// turn between the session's last main-task turn and the
// terminal SessionTerminated frame, giving weak models a narrow
// deterministic moment to persist findings to the notepad.
type CloseTurnBlock struct {
	// SystemPrompt is the full close-turn system instruction.
	// Already merged with any extension default — consumers
	// inject it verbatim. Empty when no close turn should fire.
	SystemPrompt string

	// AllowedTools narrows the tool catalogue for the close
	// turn to exactly these names. Empty falls back to the
	// regular session surface (the runtime applies no extra
	// filter). Phase 4.2.3 default for notepad close: just
	// ["notepad:append"] to keep the model focused.
	AllowedTools []string

	// MaxTurns caps the close-turn LLM iterations independent
	// of the session's regular max_turns budget. Zero falls
	// back to a runtime default (2). The close-turn budget is
	// not counted against the main task.
	MaxTurns int

	// SkipIfIdle, when true, the runtime skips the close turn
	// entirely if the session emitted zero tool calls during
	// its main task. Cheap-path shortcut for trivial sessions
	// (simple-answerer, /end at root).
	SkipIfIdle bool

	// Skip, when true, UNCONDITIONALLY suppresses the close turn
	// for this session — regardless of tool-call count. Recipe
	// children (Phase 6.1d) set this so the handoff's own
	// memory_summary stays the canonical takeaway and the runtime
	// doesn't burn a second LLM round-trip on a redundant notepad
	// append.
	Skip bool
}

// IsEmpty reports whether the block carries no actionable
// configuration. The runtime treats an empty block the same as
// "lookup returned nil" — no close turn fires.
func (b CloseTurnBlock) IsEmpty() bool {
	return b.SystemPrompt == "" && len(b.AllowedTools) == 0 && b.MaxTurns == 0 && !b.SkipIfIdle && !b.Skip
}

// CloseTurnLookup extensions resolve the on_close block for a
// session at teardown time. The runtime calls this once per
// closing session (worker, mission, root) right after the
// session's last main-task turn and before SessionTerminated.
//
// Implementations walk loaded skills and pick the most-specific
// match in this precedence:
//
//  1. Sub-agent role override (the session's spawn_role matches
//     a SubAgentRole.OnClose entry on a loaded skill).
//  2. Mission-level OnClose on a loaded dispatching skill
//     (e.g. analyst.mission.on_close.notepad).
//  3. Autoloaded tier skill (_worker / _mission) generic
//     OnClose default.
//
// Returns ({}, nil) when no loaded skill opts in — callers
// gate-check via [CloseTurnBlock.IsEmpty].
//
// Skill ext is the canonical implementor (it owns the manifest).
// Other extensions can opt in to contribute a built-in default
// (typically: the notepad extension's own per-tier default
// prompt) by composing in front of skill ext in the deps slice.
type CloseTurnLookup interface {
	ResolveCloseTurn(ctx context.Context, state SessionState, spawnSkill, spawnRole string) (CloseTurnBlock, error)
}

// WhiteboardSystemWriter extensions expose a direct in-process
// whiteboard-init path bypassing the ToolManager — the runtime
// uses it to open a mission's whiteboard from on_mission_start.
// Phase 4.2.2 §7.
type WhiteboardSystemWriter interface {
	SystemInit(ctx context.Context, state SessionState) error
}

// FrameRouter extensions handle inbound [protocol.ExtensionFrame]
// addressed to them (Frame.Extension == ext.Name()). The session's
// route loop dispatches by Extension name; each name maps to at
// most one router. Returning an error surfaces as a warning in the
// session log; the frame is considered consumed regardless.
//
// Extensions that emit ExtensionFrames but never consume them
// (plan, skill) skip this interface — only consumers implement it
// (today: whiteboard's broadcast member-side handler).
type FrameRouter interface {
	HandleFrame(ctx context.Context, state SessionState, f *protocol.ExtensionFrame) error
}

// TurnBoundaryHook extensions get a synchronous callback once per
// user-turn at the idle→active boundary, AFTER pendingInbound has
// drained into s.history and BEFORE the model resolve / prompt
// build runs. The hook owns the boundary moment: it sees the
// settled history slice + the calling session's state, and may
// perform side-effects (emit ExtensionFrames, mutate own per-
// session projection) before the new turn fires.
//
// Canonical implementor is the [compactor] extension (phase 5.2):
// runs its hybrid trigger predicate against the FrameObserver-
// maintained boundary index and either dispatches a compaction
// LLM call or returns nil. Future implementors (cron / scheduler
// in phase 6) will register additional hooks on the same shape.
//
// Concurrency: invoked synchronously on the session's Run
// goroutine — implementations may block on LLM calls but MUST
// respect ctx.Done so /cancel and turn-level deadlines unwind
// cleanly. Errors are logged warn-not-fatal — the next boundary
// retries, the new turn proceeds regardless.
//
// Ordering: hooks fire in extension-registration order. An
// implementation that reads state another hook wrote (e.g. a
// scheduler reading compactor digest) registers later in the
// runtime.Build slice.
type TurnBoundaryHook interface {
	OnTurnBoundary(ctx context.Context, state SessionState) error
}

// HistoryOwner extensions materialise the model-visible
// conversation history each turn. Phase 5.2.η refactors
// `Session.history` from a Session-owned mutable slice into an
// extension-owned cache; the [compactor] extension is the
// canonical owner.
//
// The runtime invariant is exactly-one HistoryOwner per agent —
// extensions phase wires a uniqueness assertion (see
// `pkg/runtime/extensions.go`). The empty-history case is
// reachable only via `compactor.strategy: off`.
//
// ProvideHistory returns a fresh slice; callers may append to it
// without affecting owner-internal state. Called on the session's
// Run goroutine from [Session.buildMessages] each turn.
//
// Concurrency: implementations mutate their cache from
// [FrameObserver] (also Run-goroutine for direct emits; spawn-
// pump goroutines for child frames). The owner is responsible
// for its own mutex.
//
// η.1 ships the capability + projection plumbing; the
// Session.buildMessages read path stays on `s.history` until
// η.2 flips the switch.
type HistoryOwner interface {
	ProvideHistory(ctx context.Context, state SessionState) []model.Message

	// RollbackTo drops history entries with Seq > seq. Called
	// from the session's turn loop when a /cancel or stream
	// error retires a turn — the cancelled iter's
	// consolidated assistant + tool_result entries must
	// disappear from the model's next prompt build so it
	// doesn't see an orphaned tool_call → result chain. The
	// user message that started the cancelled turn (Seq ==
	// baseline) is preserved by intent (η spec §6).
	//
	// Phase 5.2.η.3 — exposed through the interface so
	// pkg/session can call it without importing
	// pkg/extension/compactor.
	RollbackTo(ctx context.Context, state SessionState, seq int64)
}

// ContextProviderName is the tool.ToolProvider name hosting the
// synthetic context:* in-turn checkpoint tools (Stage 2 / L3). It
// lives here — in the interface package both pkg/session and the
// compactor import — so the tool-loop can identify context:* calls as
// block-exempt without importing the concrete compactor package.
const ContextProviderName = "context"

// ContextInput carries the per-iteration signals the session feeds the
// [ContextController] so it can decide whether the upcoming tool
// dispatch should be blocked. Both are read at the iteration boundary
// (between a tool round and the next model call).
type ContextInput struct {
	// RealPromptTokens is the prompt occupancy the provider reported
	// for the MOST RECENT model iteration (lastCallUsage.PromptTokens)
	// — the trigger-2 (budget-band) signal. 0 when no call has reported
	// yet, which keeps trigger 2 inert until real usage is known.
	RealPromptTokens int
	// Budget is the tier's MaxPromptTokens (0 when none is configured —
	// trigger 2 is inert, trigger 1 / manual hide still work).
	Budget int
}

// ContextDecision is the [ContextController]'s per-iteration verdict.
// Re-evaluated every iteration (NOT latched, unlike the 0.85 budget
// kill): CheckpointRequired clears the moment a checkpoint resets the
// segment counter; ContextFull clears the moment a hide drops real
// occupancy back under the band.
type ContextDecision struct {
	// CheckpointRequired blocks non-context tools until a checkpoint
	// closes the over-window segment (L3 trigger 1).
	CheckpointRequired bool
	// ContextFull blocks non-context tools until the model sheds
	// context back under the 0.80 hide band (L3 trigger 2).
	ContextFull bool
	// Inject is the system message the session folds into history on
	// the RISING edge of either block (the session owns edge-tracking
	// to avoid per-iteration spam), or "" for nothing.
	Inject string
}

// ContextController is the L3 in-turn checkpoint capability. The
// session calls EvaluateContext once per model-iteration boundary; the
// controller — the compactor, which owns the history + checkpoint
// state + config — decides whether the upcoming tool dispatch should
// be blocked (segment window blown, or real occupancy over the hide
// band) and supplies an advisory to inject. The session applies the
// verdict (sets the per-turn block flags read by dispatchToolCall) and
// injects on the rising edge.
//
// The context:* tools themselves are EXEMPT from the resulting blocks
// (the session checks [ContextProviderName]) so the model can always
// recover. Root-off / disabled is the controller's own concern — it
// returns a zero ContextDecision when checkpoints are not enabled for
// the calling session's tier.
type ContextController interface {
	EvaluateContext(ctx context.Context, state SessionState, in ContextInput) ContextDecision
}

// ToolApprovalPolicy lets an extension pre-empt a runtime-initiated
// tool-approval inquiry on the caller's behalf — answering
// "approved" immediately so the user never sees the modal. The
// mission extension implements this for the §4.6 "approve with
// tools" plan-approval option: when an ancestor mission session
// stamped MissionState.AutoApproveTools=true on its own approval
// modal, the policy hook walks the caller's parent chain and
// grants any subsequent requires_approval tool inquiry under that
// mission's umbrella.
//
// Contract: implementations are consulted in deps.Extensions order
// at the top of [Session.requestApproval]. The first non-nil
// (grantedByMissionID, ok=true) return wins — remaining extensions
// are skipped, and the caller emits an audit ExtensionFrame
// documenting the implicit grant. On ok=false the runtime falls
// through to the normal session:inquire(type=approval) path.
//
// The returned grantedByMissionID identifies the mission whose
// state granted the approval (audit-only — the runtime doesn't act
// on it beyond audit). Empty when no mission granted; non-empty
// only with ok=true.
//
// Phase 5.x — §4.6.5.
type ToolApprovalPolicy interface {
	MaybeAutoApprove(ctx context.Context, caller SessionState, tool string) (grantedByMissionID string, ok bool)
}

// InquiryPolicy lets an extension deny a session:inquire before it
// parks + bubbles to an operator — e.g. a headless cron fire that has
// no interactive operator to answer. Without this gate a cron session
// whose model calls session:inquire (clarification OR approval) parks
// forever: the request bubbles to nobody and the session hangs in
// `active`/`wait_*` until the fire timeout. The gate turns that into
// a fast structured failure the model can read + recover from.
//
// Contract: implementations are consulted in deps.Extensions order at
// the top of [Session.callInquire], BEFORE any feed/timer setup. The
// first (reason, deny=true) wins — remaining extensions are skipped
// and callInquire returns a `denied_no_operator` tool error (NOT a Go
// error). deny=false (or no policy registered) falls through to the
// normal park/bubble path.
//
// Because [Session.requestApproval] calls callInquire AFTER its own
// ToolApprovalPolicy auto-approve walk, this single gate also backstops
// the approval path: a cron tool on the fire's allow-list is
// auto-granted upstream; one that isn't reaches callInquire here and
// is denied rather than parking.
//
// Phase 6.2a.
type InquiryPolicy interface {
	MaybeDenyInquiry(ctx context.Context, caller SessionState) (reason string, deny bool)
}

// ToolResultEvent describes a tool result handed to
// [ModelInTurnAdvisor.OnToolResult] so an advisor can append in-turn
// corrective steer the model reads inline, with no separate emitted
// frame. It fires on EVERY tool result — the runtime does NOT scan the
// body to decide "is this an error" (that guessing was removed: a
// clean result can legitimately carry "is_error":true / "ok":false in
// its DATA, and "errors":null is a success). The authoritative signals
// travel as fields; the matching is the hint's job:
//
//   - a runtime-side error (emitToolError) carries the structured Code
//     plus its text in ResultText;
//   - a successful dispatch carries its raw JSON body in ResultText
//     with Code empty — INCLUDING a provider success-envelope failure
//     (e.g. a GraphQL `Cannot query field "X"` or a Hugr
//     `{"error":…,"ok":false}` query rejection), which is just a body
//     a hint's regex matches.
//
// A hint matches via tool-name glob + optional structured Code +
// optional regex over ResultText — see [skill.Hint].
type ToolResultEvent struct {
	// Tool is the fully-qualified tool name as dispatched
	// (e.g. "hugr-main:data-inline_graphql_result").
	Tool string
	// Code is the structured ToolError.Code for a runtime-side error
	// ("not_found" / "timeout" / "context_budget" / …); empty for a
	// successful dispatch (where the body is in ResultText).
	Code string
	// ResultText is the tool result text the hint regex matches: the
	// runtime error message for a runtime-side error, or the raw
	// result JSON for a successful dispatch.
	ResultText string
}

// ModelInTurnAdvisor is the umbrella capability for an extension that
// contributes content INTO the active turn — near the model's
// decision point, ephemerally (render-time, not a persisted frame) —
// vs. observing the turn after the fact. It exposes typed variations;
// an implementer returns "" from any variation it does not serve.
//
// Naming: -Advisor (not -Actioner) because the contract is pull-pure
// — the extension RETURNS its contribution, the session OWNS applying
// it (mirrors [ToolPolicyAdvisor]). A future MUTATING variation
// (e.g. pre-call arg fixup) would strain "advisor"; split a separate
// capability then rather than bending this one.
//
// Variations (Phase 6.x):
//
//   - TurnPreamble — the dynamic-skill advertise block, injected just
//     before the last user message in [Session.buildMessages] instead
//     of baked into the system prompt. Two wins: recency (weak models
//     attend near the ask, not the top of a long system prompt) and
//     prompt cache (the system prompt stays stable/cacheable while the
//     volatile advertise rides after the cache boundary).
//   - OnToolResult — guidance appended inline to a tool result whose
//     content matches a hint (manifest `metadata.hugen.hints` of type
//     on_tool_result). It fires on EVERY result — success or failure
//     alike — and the hint's tool glob + optional Code + optional regex
//     over [ToolResultEvent.ResultText] decide the match; the runtime
//     no longer pre-classifies error-vs-success from the body. The
//     session folds the returned text into the result content the model
//     reads, with no separate emitted frame.
//
// Contract: implementations are consulted in deps.Extensions order.
// TurnPreamble contributions are joined; OnToolResult contributions are
// joined (the session de-dupes / caps). All are pure — the session owns
// where the text lands.
type ModelInTurnAdvisor interface {
	// TurnPreamble returns the ephemeral block to inject before the
	// last user message this turn, or "" for nothing.
	TurnPreamble(ctx context.Context, state SessionState) string
	// OnToolResult returns guidance to append to a tool result whose
	// content matched an on_tool_result hint, or "" for nothing. Fed
	// every tool result — runtime error and successful dispatch alike.
	OnToolResult(ctx context.Context, state SessionState, ev ToolResultEvent) string
}

// TurnFinalizeGate lets an extension veto a session's turn
// finalization — the model emitted a final message with no tool
// call, so the turn would normally retire — and supply a
// continuation prompt the runtime injects before re-iterating the
// SAME session. This is the planner-gate primitive (Phase 6.x): the
// mission ext implements it so a planner cannot end its turn until it
// has submitted a plan the user approved via
// `mission:validate_and_approve`, while keeping the planner's
// in-session context (a re-plan-from-scratch respawn would discard
// it). The same primitive holds a Do worker / checker / synthesizer
// from ending its turn without a parseable terminal `handoff` fence —
// a weak model that "thought" its answer but never wrote the fence
// (e.g. a thinking-model whose tokens all went to reasoning) gets
// re-prompted in-session to emit it, instead of wedging the mission's
// waitForRefs forever.
//
// finalText is the assistant's just-assembled final message for this
// iteration (empty when the model produced no visible content) — a
// gate that decides by message CONTENT (the worker handoff gate)
// inspects it; a gate that decides by tool-call STATE (the planner's
// validate_and_approve submission) ignores it.
//
// Distinct from [ModelInTurnAdvisor]: that capability is pull-pure
// (it returns text the session decides where to place), whereas this
// one is MUTATING / flow-controlling — its verdict changes whether
// the turn retires or loops. The two were deliberately split rather
// than bending the "advisor" contract.
//
// Contract:
//   - allow=true (the default for any session without a declared
//     finalize condition) lets the turn retire normally; continuation
//     is ignored. Every non-gated session — workers, root chat,
//     non-mission sessions — returns allow=true.
//   - allow=false vetoes finalize: the runtime injects continuation
//     as a system reminder into the SAME session's history and starts
//     a fresh model iteration instead of retiring. The session enforces
//     a hard retry backstop so a gate that never allows can't loop
//     forever — past the cap the turn retires regardless.
//
// Implementations are consulted in deps.Extensions order; the first
// to veto wins (continuation taken from it). A gate that wants to
// terminate the session (e.g. the user aborted the plan) returns
// allow=true and arms its own teardown via state — the gate does not
// own session lifecycle.
type TurnFinalizeGate interface {
	GateTurnFinalize(ctx context.Context, state SessionState, finalText string) (continuation string, allow bool)
}
