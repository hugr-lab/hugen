// Package compactor implements the phase 5.2 content-aware
// history compactor: a capability-bag extension that summarises
// older transcript content at turn boundaries when configured
// thresholds (token + turn count) trip, persists the digest as
// an [protocol.ExtensionFrame], and renders it as a system-
// prompt section on subsequent turns.
//
// Append-only persistence; the session_events log is the ground
// truth — compaction only changes what the MODEL sees in the
// prompt. UI adapters render the full unmodified transcript and
// draw an inline marker at the digest boundary.
//
// See design/004-runtime-post-phase-i/phase-5.2-compactor-spec.md
// for the full design. This file owns the state shape; sibling
// files own behaviour (extension.go = capability wiring,
// trigger.go = predicate, recovery.go = replay,
// advertise.go = Block C, frame_observer.go = boundary index).
package compactor

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
)

// StateKey is the [extension.SessionState] key the extension
// stores its per-session [*CompactorState] handle under.
const StateKey = "compactor"

// providerName doubles as the extension's [Extension.Name] and
// the routing discriminator on emitted ExtensionFrames.
const providerName = "compactor"

// CurrentPayloadVersion is the wire-schema version emitted in
// [DigestPayload.Version]. Recovery ignores frames with
// Version > CurrentPayloadVersion (forward-compat). Bump on any
// incompatible shape change.
const CurrentPayloadVersion = 1

// ExtensionFrame op names emitted under
// [protocol.CategoryOp].
const (
	OpDigestSet   = "digest_set"
	OpDigestClear = "digest_clear"
)

// DigestPayload is the wire representation of a compaction
// snapshot. Latest-wins on Recovery replay: only the most-recent
// `digest_set` op contributes to the in-memory projection (older
// rows are skipped after replay). See spec §3.3 / §5.5 for the
// incremental SummaryBlocks model + cap-driven collapse.
type DigestPayload struct {
	// Version of the payload schema. Bump on incompatible
	// shape changes so Recovery can ignore stale rows.
	Version int `json:"version"`

	// Iteration counter — increments on every successful
	// compaction. Latest wins on Recovery replay.
	Iteration int `json:"iteration"`

	// CutoffSeq is the seq of the LAST frame INCLUDED in this
	// digest. Frames with seq > CutoffSeq are the preserved
	// recent window — they stay in the live history slice
	// unmodified. Always aligned to a turn boundary (just
	// before a user_message). See spec §3.5.
	CutoffSeq int64 `json:"cutoff_seq"`

	// CompactedAtSeq is the max seq at the moment compaction
	// fired. May be > CutoffSeq because of the preserved
	// recent window. Audit / debug field; not load-bearing.
	CompactedAtSeq int64 `json:"compacted_at_seq"`

	// KeptVerbatim — high-signal entries the model needs
	// exactly as written: user_message + final agent_message
	// text + a curated subset of system/error messages.
	// Accumulated across iterations until cap-driven
	// re-summary prunes (see spec §5.5).
	KeptVerbatim []KeptSection `json:"kept_verbatim"`

	// SummaryBlocks — one block per compaction iteration.
	// Renders in Block C as a chronological list. Cap-driven
	// re-summary collapses all blocks into a single block
	// when the total exceeds digest_max_tokens.
	SummaryBlocks []SummaryBlock `json:"summary_blocks"`

	// SubagentRefs — handoff refs and SubagentResult reasons
	// surfaced across all compacted iterations.
	SubagentRefs []SubagentRef `json:"subagent_refs"`

	// BuiltAt — timestamp for debug + audit. NOT load-bearing
	// for replay correctness.
	BuiltAt time.Time `json:"built_at"`

	// UIMarkerEnabled echoes the resolved config flag at compaction
	// time so adapters can read the marker toggle directly off the
	// `digest_set` frame — without waiting for the next liveview
	// status payload. Defaults to true; operators set
	// `compactor.ui_marker.enabled: false` to suppress.
	UIMarkerEnabled bool `json:"ui_marker_enabled"`
}

// SummaryBlock is one LLM-generated narrative covering the
// tool-call sequence in [From, To] seq range. Multiple blocks
// chain chronologically until cap-driven collapse merges them.
type SummaryBlock struct {
	// Iter matches DigestPayload.Iteration at the time this
	// block was written.
	Iter int `json:"iter"`
	// From is the first seq covered (exclusive of prior
	// block's To, or 0 for the very first block).
	From int64 `json:"from"`
	// To is the last seq covered (== CutoffSeq at that iter).
	To int64 `json:"to"`
	// Text is the LLM-generated narrative.
	Text string `json:"text"`
}

// KeptSection is a verbatim entry the model continues to see
// exactly as originally written. Used for user_message,
// agent_message{Final,Consolidated}, terminal Error,
// system_message, and inquiry Q/A pairs.
type KeptSection struct {
	// Kind is the verbatim discriminator (frame Kind or a
	// composite label like "inquiry_qa").
	Kind string `json:"kind"`
	// Author is the participant id who produced the frame
	// (user / agent / system).
	Author string `json:"author"`
	// Seq is the original frame seq the section came from.
	Seq int64 `json:"seq"`
	// Text is the verbatim content.
	Text string `json:"text"`
}

// SubagentRef carries the addressing of a child session
// referenced earlier in the compacted range, so the model can
// continue to mention "session=… reason=…" without re-reading
// the original SubagentResult frames.
type SubagentRef struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
	Wave      string `json:"wave,omitempty"`
	Role      string `json:"role,omitempty"`
}

// CompactorState is the per-session typed handle the compactor
// stores in [extension.SessionState] under [StateKey]. Read by
// Advertiser (Block C), trigger predicate, adapter
// SnapshotSession projection, and (η.1+) ProvideHistory.
//
// Concurrency: every method acquires its own mutex (mu for
// digest, historyMu for the history projection, boundary owns
// its own). The mutation paths are SetDigest (called from
// compaction pipeline + Recovery replay), updates from
// FrameObserver (boundary index + history projection), and full
// rebuild from Recover.
type CompactorState struct {
	mu     sync.Mutex
	digest *DigestPayload

	// boundary tracks user-message seqs + a running token
	// estimate. Maintained by [Extension.OnFrameEmit]
	// (FrameObserver capability) so the trigger predicate
	// runs in O(1) at boundary time.
	boundary boundaryTracker

	// historyMu guards [history]. Held briefly during append +
	// snapshot — never across LLM / I/O.
	historyMu sync.Mutex

	// history is the incrementally-maintained projection of the
	// session's persisted events onto [model.Message] shape.
	// η.1 ships the field + appender + ProvideHistory plumbing;
	// the Session.buildMessages read path stays on the legacy
	// s.history until η.2 flips the switch.
	history []HistoryEntry

	// advertiseMu guards [advertiseTokens]. Held briefly during
	// Set / Get — never across LLM / I/O.
	advertiseMu sync.Mutex

	// advertiseTokens caches the estimated token weight of the
	// last [Extension.AdvertiseSystemPrompt] render so
	// [Extension.ReportStatus] can surface the number without
	// re-running the renderer. Phase 5.2 (context-budget β).
	advertiseTokens int

	// cpMu guards the L3 in-turn checkpoint state below. Held briefly
	// during checkpoint/hide/expand/rollback ops + segment reads —
	// never across LLM / I/O. Separate from historyMu so a segment-
	// token read (which snapshots history under historyMu) doesn't
	// contend with a checkpoint list read.
	cpMu sync.Mutex
	// checkpoints is the ordered list of model-authored segment markers
	// (oldest → newest). Each closes the segment ending at its Seq; the
	// range (prev.Seq, this.Seq] is the segment it can hide / roll back.
	checkpoints []Checkpoint
	// lastCheckpointSeq is the Seq of the most recent checkpoint (0 when
	// none). The current (open) segment is every cached history entry
	// with Seq > lastCheckpointSeq — always fully visible (§6.2), which
	// is what makes the segment counter hide-immune (§6.3).
	lastCheckpointSeq int64
	// cpCounter mints stable "cp-N" ids — monotonic across the session
	// so a rolled-back / hidden id never reappears with new content.
	cpCounter int
	// occReal / occBudget / occHideThreshold cache the latest REAL
	// context occupancy (lastCallUsage.PromptTokens), the tier budget,
	// and the 0.80 hide threshold — stamped by the controller every
	// EvaluateContext. The model has no token counter of its own, so
	// the context:* tool results + nudges surface these to make a
	// hide/checkpoint decision rational (low fill → don't hide; near the
	// band → shed). Atomics: written + read on the Run goroutine,
	// lock-free keeps the hot read path off cpMu.
	occReal          atomic.Int64
	occBudget        atomic.Int64
	occHideThreshold atomic.Int64
	// preambleFloorVal is the seq boundary between the task PREAMBLE
	// (the worker's first user_message + handoff contract + any system
	// setup + the model's pre-tool planning) and the model-generated
	// WORK that follows. Entries at or below it are NEVER collapsed by a
	// hide and never counted toward the segment window — only work after
	// the first tool call is sheddable. Computed once (the boundary is
	// fixed — preamble entries never roll back) and cached. Guards
	// against hide swallowing the task definition (the dogfood failure:
	// a researcher hid cp-1, lost "fill research.md / data-model.md",
	// and submitted empty stubs). preambleFloorSet flips on first
	// computation; 0 with set=false means "no tool work yet".
	preambleFloorVal int64
	preambleFloorSet bool
}

// SetAdvertiseTokens records the cached estimate. Called from
// [Extension.AdvertiseSystemPrompt] after each render.
func (s *CompactorState) SetAdvertiseTokens(n int) {
	s.advertiseMu.Lock()
	defer s.advertiseMu.Unlock()
	s.advertiseTokens = n
}

// AdvertiseTokens returns the cached estimate from the last
// AdvertiseSystemPrompt call. Zero until the first render.
func (s *CompactorState) AdvertiseTokens() int {
	s.advertiseMu.Lock()
	defer s.advertiseMu.Unlock()
	return s.advertiseTokens
}

// HistoryTokens sums [EstimateTokens] over the message content
// of every entry currently in the owned history cache. Lives
// on the hot read path of liveview's status emit — keeps the
// locked section narrow by snapshotting first.
func (s *CompactorState) HistoryTokens() int {
	entries := s.historySnapshot()
	if len(entries) == 0 {
		return 0
	}
	total := 0
	for _, ent := range entries {
		total += estimateMessageTokens(ent.Message)
	}
	return total
}

// HistoryEntry is one row of the compactor's owned history
// projection. Seq + Timestamp come from the source frame envelope;
// Message is the projected model.Message ready to feed into the
// LLM call. A future multi-contributor merge (see η spec §2.1)
// would sort across owners by Timestamp.
type HistoryEntry struct {
	Seq       int64
	Timestamp time.Time
	Message   model.Message
}

// boundaryTracker is the FrameObserver-maintained running
// projection of "where could the compactor cut" + accumulated
// prompt-token estimate.
type boundaryTracker struct {
	mu sync.Mutex
	// userMessageSeqs is the chronological list of seqs for
	// every persisted user_message on this session. Used to
	// align CutoffSeq with a turn boundary.
	userMessageSeqs []int64
	// estimatedTokens is the running sum of estimated tokens
	// across persisted-so-far frames that contribute to the
	// model prompt. Used by the token-budget trigger limb.
	estimatedTokens int
}

// Digest returns a snapshot pointer to the latest persisted
// digest, or nil if no compaction has fired yet. Callers MUST
// treat the returned value as read-only.
func (s *CompactorState) Digest() *DigestPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.digest
}

// SetDigest replaces the in-memory digest snapshot. Called from
// the compaction pipeline (after emit) and from Recovery replay.
func (s *CompactorState) SetDigest(d *DigestPayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.digest = d
}

// ClearDigest drops the digest snapshot. Called by Recovery on
// digest_clear, by `/compactor reset`, and by test fixtures.
func (s *CompactorState) ClearDigest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.digest = nil
}

// BoundaryCount returns the number of user-messages observed on
// this session so far. Used by the trigger predicate's
// preserved-window check.
func (s *CompactorState) BoundaryCount() int {
	s.boundary.mu.Lock()
	defer s.boundary.mu.Unlock()
	return len(s.boundary.userMessageSeqs)
}

// BoundaryAt returns the seq of the i-th user_message
// (0-indexed). Panics on out-of-range; callers gate via
// [CompactorState.BoundaryCount].
func (s *CompactorState) BoundaryAt(i int) int64 {
	s.boundary.mu.Lock()
	defer s.boundary.mu.Unlock()
	return s.boundary.userMessageSeqs[i]
}

// EstimatedPromptTokens returns the running token estimate.
func (s *CompactorState) EstimatedPromptTokens() int {
	s.boundary.mu.Lock()
	defer s.boundary.mu.Unlock()
	return s.boundary.estimatedTokens
}

// appendBoundary records a user_message seq + token delta on
// the running projection. Internal — called only from
// [Extension.OnFrameEmit].
func (s *CompactorState) appendBoundary(seq int64, tokenDelta int) {
	s.boundary.mu.Lock()
	defer s.boundary.mu.Unlock()
	if seq > 0 {
		s.boundary.userMessageSeqs = append(s.boundary.userMessageSeqs, seq)
	}
	s.boundary.estimatedTokens += tokenDelta
}

// addTokens bumps the running token estimate without recording a
// new boundary — used for non-user frames that still contribute
// to prompt size.
func (s *CompactorState) addTokens(delta int) {
	if delta <= 0 {
		return
	}
	s.boundary.mu.Lock()
	defer s.boundary.mu.Unlock()
	s.boundary.estimatedTokens += delta
}

// appendHistory records one projected entry. Internal — called
// only from [Extension.OnFrameEmit] (live path) and from
// [Extension.Recover] (replay).
func (s *CompactorState) appendHistory(entry HistoryEntry) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.history = append(s.history, entry)
}

// resetHistory replaces the projection wholesale. Internal —
// called only from [Extension.Recover] after the second pass
// builds a fresh slice.
func (s *CompactorState) resetHistory(entries []HistoryEntry) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.history = entries
}

// historySnapshot returns a fresh copy of the projected entries.
// Callers may mutate the returned slice freely.
func (s *CompactorState) historySnapshot() []HistoryEntry {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if len(s.history) == 0 {
		return nil
	}
	out := make([]HistoryEntry, len(s.history))
	copy(out, s.history)
	return out
}

// snapToPairSafeHead returns the largest index ≤ head at which the
// preserved tail can begin without orphaning a tool_result from its
// tool_call. A model that emits N parallel tool_calls in one
// assistant message (Gemini does this) produces a CONTIGUOUS run of
// N RoleTool entries right after it (SubagentStarted is not projected,
// so nothing interleaves — see history.go); walking back over that
// run lands on the owning assistant, so the whole [assistant][N
// results] group stays on one side of the cut. Strict providers
// reject a function_response that does not follow its function_call,
// so this is load-bearing, not cosmetic.
//
// The head < len guard keeps a degenerate empty-tail head (== len)
// safe: an empty preserved tail has no pair to split.
func snapToPairSafeHead(entries []HistoryEntry, head int) int {
	for head > 0 && head < len(entries) && entries[head].Message.Role == model.RoleTool {
		head--
	}
	return head
}

// firstUserMessageIndex returns the index of the first RoleUser entry
// (the turn's task brief), or -1 when none is present. Used to pin
// the brief ahead of a window prune so a long worker turn never loses
// what it was asked to do.
func firstUserMessageIndex(entries []HistoryEntry) int {
	for i := range entries {
		if entries[i].Message.Role == model.RoleUser {
			return i
		}
	}
	return -1
}

// pruneWindow keeps the most-recent entries (≈ `limit`). Used by
// [StrategyWindow]; called from [Extension.OnFrameEmit] after every
// append. Two invariants beyond the raw FIFO:
//
//   - The window head is snapped pair-safe so the model-visible
//     history never begins on a tool_result orphaned from its
//     tool_call (the window may float a few entries above `limit` to
//     keep a tool group whole — never below).
//   - The first user_message (task brief) is pinned ahead of the
//     window so a long single-turn worker never loses its task +
//     handoff contract. The window otherwise carries the most-recent
//     `limit-1` entries, so the total stays ≈ `limit`.
func (s *CompactorState) pruneWindow(limit int) {
	if limit <= 0 {
		return
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	n := len(s.history)
	if n <= limit {
		return
	}
	// Leave one slot for the pinned brief, then snap the head back
	// over any trailing tool_result run.
	head := snapToPairSafeHead(s.history, n-(limit-1))
	pin := firstUserMessageIndex(s.history)
	var keep []HistoryEntry
	if pin >= 0 && pin < head {
		keep = make([]HistoryEntry, 0, 1+(n-head))
		keep = append(keep, s.history[pin])
		keep = append(keep, s.history[head:]...)
	} else {
		// Brief already inside the window (or none recorded) — the
		// snapped tail is the whole kept slice.
		keep = make([]HistoryEntry, n-head)
		copy(keep, s.history[head:])
	}
	s.history = keep
}

// RollbackTo drops history entries with Seq > seq. Used by
// [Session.rollbackTurn] (η.3) to undo cache appends that
// happened after a /cancel or stream error during the
// just-aborted turn. The user message itself (its Seq equals
// the rollback baseline) is preserved by intent — see η spec
// §6 for the cancel-semantics rationale.
func (s *CompactorState) RollbackTo(seq int64) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if len(s.history) == 0 {
		return
	}
	keep := s.history[:0]
	for _, ent := range s.history {
		if ent.Seq <= seq {
			keep = append(keep, ent)
		}
	}
	out := make([]HistoryEntry, len(keep))
	copy(out, keep)
	s.history = out
}

// pruneToCutoff drops entries with Seq <= cutoff. Used by
// [StrategySummarize]; called from [Extension.compactWithConfig]
// after a successful digest_set emit so the live history matches
// what Block C carries.
func (s *CompactorState) pruneToCutoff(cutoff int64) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if len(s.history) == 0 {
		return
	}
	keep := s.history[:0]
	for _, ent := range s.history {
		if ent.Seq > cutoff {
			keep = append(keep, ent)
		}
	}
	// Realloc so the backing array shrinks; otherwise a long-
	// running session leaks the original capacity forever.
	out := make([]HistoryEntry, len(keep))
	copy(out, keep)
	s.history = out
}
