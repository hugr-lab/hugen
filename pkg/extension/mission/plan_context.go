package mission

import (
	"sync"
	"time"
)

// planContextSoftCap is the FIFO threshold the plan_context journal
// trims to once its character count exceeds the cap. Roughly
// 2500 characters ≈ 600 tokens for English text — sized so the
// rendered journal fits well under the planner's input budget on
// weak models (Gemma 4-26B with max_tokens=8192 leaves room for
// the rest of the prompt). Phase 5.2 compactor's per-role
// override is the path to richer summarisation; this is a hard
// fallback when the compactor is disabled or hasn't kicked in.
const planContextSoftCap = 2500

// PlanContextEntry is one row in the per-mission plan_context
// journal — a compact projection of a single handoff's
// memory_summary. The Phase column tags where the entry came from
// (`plan` for planner handoffs, `do` for worker handoffs,
// `verdict` for checker handoffs, `synthesis` for the
// synthesizer's, `user-followup` for Phase-E followups, etc.) so
// the renderer can group / prefix lines deterministically.
//
// Append-only: the journal is never updated in place; new
// information lands as fresh entries. FIFO trim drops the oldest
// entries when the rendered prose exceeds the soft cap.
type PlanContextEntry struct {
	// Iteration is the planner-loop iteration this handoff
	// belonged to. 0 means "outside the planner loop" (e.g.
	// user-followup before iteration 1).
	Iteration int

	// Phase is the row's high-level bucket: plan / do / verdict /
	// synthesis / user-followup. Other extensions may carry
	// additional buckets in future phases.
	Phase string

	// Role is the sub_agents.name the producing session was
	// spawned under. Empty when the producer wasn't a worker —
	// e.g. user-followup entries.
	Role string

	// Name is the producing session's short addressing identifier
	// (the SpawnSpec.Name passed at spawn). Empty for system
	// entries like user-followup.
	Name string

	// Wave is the wave label this handoff belonged to. Empty for
	// non-wave entries.
	Wave string

	// Summary is the short prose that ships to downstream phase
	// roles. Usually the producer's memory_summary; for
	// user-followup it's the user's message verbatim.
	Summary string

	// CreatedAt stamps when the entry was appended. Renders into
	// no model-visible output today; future formatters may
	// surface it.
	CreatedAt time.Time
}

// PlanContext is the per-mission append-only memory-summary
// journal. Reads are by-iteration / list-all; writes are
// append-only via [PlanContext.Append] (or
// [PlanContext.AppendHandoff] for the canonical "extract from a
// handoff" path).
//
// Concurrency: append is goroutine-safe under an internal mutex.
// Snapshot reads return a fresh slice the caller can iterate
// without holding any lock.
type PlanContext struct {
	mu      sync.Mutex
	entries []PlanContextEntry
}

// NewPlanContext returns a zero-value PlanContext ready for
// appending.
func NewPlanContext() *PlanContext {
	return &PlanContext{}
}

// Append records entry to the journal and triggers a FIFO trim
// when the total rendered prose exceeds the soft cap. CreatedAt
// is filled with [nowFn] when zero.
func (pc *PlanContext) Append(entry PlanContextEntry) {
	if pc == nil {
		return
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = nowFn()
	}
	pc.mu.Lock()
	pc.entries = append(pc.entries, entry)
	pc.trimLocked()
	pc.mu.Unlock()
}

// AppendHandoff is the canonical extract-from-handoff entry
// point. Pulls the memory_summary off h, tags the row with the
// canonical phase bucket inferred from the handoff's kind +
// wave label, and appends. No-op when h.MemorySummary is empty
// — handoffs without a summary don't contribute to the journal.
//
// Iteration is the planner-loop iteration the handoff belonged
// to; pass 0 from contexts that aren't inside the loop (e.g. the
// scenario harness pre-seeds before the loop starts).
func (pc *PlanContext) AppendHandoff(iteration int, wave string, h Handoff) {
	if pc == nil || h.MemorySummary == "" {
		return
	}
	pc.Append(PlanContextEntry{
		Iteration: iteration,
		Phase:     phaseForHandoff(h, wave),
		Role:      h.Subagent.Role,
		Name:      h.Subagent.Name,
		Wave:      wave,
		Summary:   h.MemorySummary,
	})
}

// List returns a fresh snapshot of every recorded entry in
// insertion order. Safe to iterate without holding the journal's
// lock.
func (pc *PlanContext) List() []PlanContextEntry {
	if pc == nil {
		return nil
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	out := make([]PlanContextEntry, len(pc.entries))
	copy(out, pc.entries)
	return out
}

// Len reports the current journal size — useful for telemetry +
// observation tests that don't need the full snapshot.
func (pc *PlanContext) Len() int {
	if pc == nil {
		return 0
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return len(pc.entries)
}

// trimLocked drops the oldest entries while the rendered prose
// exceeds the soft cap. Caller holds pc.mu.
func (pc *PlanContext) trimLocked() {
	total := 0
	for _, e := range pc.entries {
		total += len(e.Summary)
	}
	for total > planContextSoftCap && len(pc.entries) > 1 {
		// drop oldest; leave at least one entry so the journal
		// never collapses to empty when a single huge summary
		// blows the cap on its own.
		dropped := pc.entries[0]
		pc.entries = pc.entries[1:]
		total -= len(dropped.Summary)
	}
}

// phaseForHandoff infers the plan_context.phase column from the
// handoff's kind + wave-label prefix. Drives the template
// renderer's per-phase grouping (planner reads "plan", checker
// reads "verdict", synthesis "synthesis", regular workers "do").
func phaseForHandoff(h Handoff, wave string) string {
	switch h.Kind {
	case KindPlan:
		return "plan"
	case KindVerdict:
		return "verdict"
	case KindSynthesis:
		return "synthesis"
	}
	switch {
	case len(wave) >= len(plannerWaveLabelPrefix) && wave[:len(plannerWaveLabelPrefix)] == plannerWaveLabelPrefix:
		return "plan"
	case len(wave) >= len(checkerWaveLabelPrefix) && wave[:len(checkerWaveLabelPrefix)] == checkerWaveLabelPrefix:
		return "verdict"
	case wave == synthesisWaveLabel:
		return "synthesis"
	default:
		return "do"
	}
}
