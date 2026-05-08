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
