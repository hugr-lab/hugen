package mission

import (
	"fmt"
	"strings"
)

// stagedDiff is the in-progress planner-emitted diff awaiting an
// approval modal verdict. Lives on MissionState.pendingDiff between
// the planner-emit and the modal-close.
type stagedDiff struct {
	// Diff is the planner's ac_add + ac_update payload.
	Diff ACDiff

	// Iter is the planner iteration that produced the diff. Used to
	// stamp added_at_iter on the resulting rows when the diff
	// commits.
	Iter int

	// Origin is the canonical origin tag for ac_add entries that
	// don't carry their own (planner_iter_N for planner-emit,
	// user_refine for refine-loop output).
	Origin string

	// Reason is the planner's reapproval_reason text (when present)
	// — surfaced to the user in the modal alongside the diff.
	Reason string
}

// SeedAC bulk-adds rows at iter 0 from manifest / user_initial input.
// Returns the assigned ids in input order so callers can correlate
// (e.g. manifest template index → ac id for downstream rendering).
//
// origin should be one of OriginManifest / OriginUserInitial — other
// origins go through StagePlannerDiff → CommitStagedDiff (planner /
// refine path).
//
// Empty / whitespace-only statements are dropped silently (no error
// — manifests with empty entries should validate at load time).
func (m *MissionState) SeedAC(items []ACAddSpec, origin string) []string {
	if len(items) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(items))
	for _, item := range items {
		stmt := strings.TrimSpace(item.Statement)
		if stmt == "" {
			ids = append(ids, "")
			continue
		}
		m.nextACID++
		id := fmt.Sprintf("ac-%d", m.nextACID)
		row := AcceptanceCriterion{
			ID:          id,
			Statement:   stmt,
			Origin:      origin,
			Status:      ACUnsatisfied,
			AddedAtIter: 0,
		}
		m.ac = append(m.ac, row)
		ids = append(ids, id)
	}
	return ids
}

// StagePlannerDiff stores the planner's diff as pending, awaiting
// approval-modal close. Returns an error if the diff fails shape
// validation, references unknown ids, or attempts to mutate a row
// that's already dropped.
//
// origin is stamped on resulting ac_add rows (PlannerOriginAt(iter)
// for planner-emit, OriginUserRefine for refine-loop).
//
// reason is the planner's reapproval_reason free-form prose — passed
// through verbatim into the modal payload (§3.6).
func (m *MissionState) StagePlannerDiff(diff ACDiff, iter int, origin, reason string) error {
	if err := ValidatePlannerDiff(diff); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkIDsLocked(diff.Update); err != nil {
		return err
	}
	m.pendingDiff = &stagedDiff{
		Diff:   diff,
		Iter:   iter,
		Origin: origin,
		Reason: strings.TrimSpace(reason),
	}
	return nil
}

// PendingDiff returns a copy of the staged diff, or nil when none is
// staged. Used by the approval-modal builder to surface staged
// changes to the user.
func (m *MissionState) PendingDiff() *ACDiff {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingDiff == nil {
		return nil
	}
	out := ACDiff{
		Add:    append([]ACAddSpec(nil), m.pendingDiff.Diff.Add...),
		Update: append([]ACUpdateSpec(nil), m.pendingDiff.Diff.Update...),
	}
	return &out
}

// PendingDiffReason returns the planner's reapproval_reason string
// captured at staging time. Empty when nothing is staged or the
// planner didn't supply one.
func (m *MissionState) PendingDiffReason() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingDiff == nil {
		return ""
	}
	return m.pendingDiff.Reason
}

// CommitStagedDiff applies the staged planner diff (and optional
// user-refine extra) to state.AC. Clears the staging slot on
// success. The refine extra is validated as a planner-channel diff
// (§3.2.2 user_refine = planner authority inside the modal).
//
// Returns the list of newly-minted ids (in order: staged.Add entries
// first, then extra.Add entries) so the caller can echo them in
// telemetry / modal feedback.
//
// Idempotent against an empty pendingDiff: returns nil when nothing
// is staged.
func (m *MissionState) CommitStagedDiff(extra ACDiff) ([]string, error) {
	if err := ValidatePlannerDiff(extra); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingDiff == nil && extra.IsEmpty() {
		return nil, nil
	}
	// Build the combined diff: staged first, then extra.
	combined := ACDiff{}
	stagedIter := 0
	stagedOrigin := OriginUserRefine
	if m.pendingDiff != nil {
		combined.Add = append(combined.Add, m.pendingDiff.Diff.Add...)
		combined.Update = append(combined.Update, m.pendingDiff.Diff.Update...)
		stagedIter = m.pendingDiff.Iter
		stagedOrigin = m.pendingDiff.Origin
	}
	combined.Add = append(combined.Add, extra.Add...)
	combined.Update = append(combined.Update, extra.Update...)
	if err := m.checkIDsLocked(combined.Update); err != nil {
		return nil, err
	}
	added := make([]string, 0, len(combined.Add))
	// Apply Add entries first so any Update entries that reference
	// freshly-minted ids resolve cleanly.
	stagedAddCount := 0
	if m.pendingDiff != nil {
		stagedAddCount = len(m.pendingDiff.Diff.Add)
	}
	for i, a := range combined.Add {
		stmt := strings.TrimSpace(a.Statement)
		if stmt == "" {
			added = append(added, "")
			continue
		}
		origin := strings.TrimSpace(a.Origin)
		if origin == "" {
			if i < stagedAddCount {
				origin = stagedOrigin
			} else {
				origin = OriginUserRefine
			}
		}
		iter := stagedIter
		if i >= stagedAddCount {
			iter = stagedIter // user_refine entries adopt the staging iter — they ride the same approval gate
		}
		m.nextACID++
		id := fmt.Sprintf("ac-%d", m.nextACID)
		m.ac = append(m.ac, AcceptanceCriterion{
			ID:          id,
			Statement:   stmt,
			Origin:      origin,
			Status:      ACUnsatisfied,
			AddedAtIter: iter,
		})
		added = append(added, id)
	}
	for _, u := range combined.Update {
		if err := m.applyUpdateLocked(u, stagedIter, "planner_iter_"+itoa(stagedIter)); err != nil {
			// Validation already ran upfront; only path here is a
			// race on AC list shape. Return so caller can roll back
			// at the planner-loop level if needed.
			return nil, err
		}
	}
	m.pendingDiff = nil
	return added, nil
}

// DiscardStagedDiff clears the staged diff without applying it.
// Called by the runtime when the user rejects the approval modal.
// State.AC is left untouched; the planner sees the rejected diff in
// plan_context.control.rejected_diff on its next iter.
func (m *MissionState) DiscardStagedDiff() *ACDiff {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingDiff == nil {
		return nil
	}
	out := ACDiff{
		Add:    append([]ACAddSpec(nil), m.pendingDiff.Diff.Add...),
		Update: append([]ACUpdateSpec(nil), m.pendingDiff.Diff.Update...),
	}
	m.pendingDiff = nil
	return &out
}

// ApplyStatusOnly applies a slice of status-only ac_update entries
// — used by the checker (§3.5) and by planner status-only diffs
// (§3.2 status-only ac_update without contract fields). Bypasses
// staging because status updates aren't contract changes.
//
// evidenceSource is the free-form source label runtime stamps on
// LastEvidence when an entry omits its own Evidence string.
// Examples: "checker iter-2", "planner iter-3 status-only".
//
// Entries carrying Statement / Drop are rejected — call
// StagePlannerDiff for those.
func (m *MissionState) ApplyStatusOnly(updates []ACUpdateSpec, iter int, evidenceSource string) error {
	if len(updates) == 0 {
		return nil
	}
	for i, u := range updates {
		if u.IsContractChange() {
			return fmt.Errorf("ac_update[%d]: ApplyStatusOnly rejects contract changes (statement / drop); route through StagePlannerDiff", i)
		}
		if u.Status == "" {
			return fmt.Errorf("ac_update[%d]: status must be set for ApplyStatusOnly", i)
		}
		switch u.Status {
		case ACUnsatisfied, ACSatisfied:
			// ok
		default:
			return fmt.Errorf("ac_update[%d]: status=%q is not one of unsatisfied/satisfied", i, u.Status)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkIDsLocked(updates); err != nil {
		return err
	}
	for _, u := range updates {
		if err := m.applyUpdateLocked(u, iter, evidenceSource); err != nil {
			return err
		}
	}
	return nil
}

// ApplyWorkerSatisfies applies the worker handoff's `satisfies: [...]`
// shorthand: for each id, mark status=satisfied with synthesised
// evidence "worker {role} handoff iter-{iter} wave-{wave}". No-op for
// already-satisfied rows (preserves the original satisfaction
// iteration / evidence — worker can't "re-satisfy" an already-met AC).
//
// Unknown ids are logged via the returned slice but do not abort —
// worker handoffs are best-effort hints, not authoritative.
func (m *MissionState) ApplyWorkerSatisfies(ids []string, iter int, role, wave string) (applied, unknown []string) {
	if len(ids) == 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		idx := m.findIDLocked(id)
		if idx < 0 {
			unknown = append(unknown, id)
			continue
		}
		row := &m.ac[idx]
		if row.Status == ACSatisfied || row.Status == ACDropped {
			// Worker can't override a checker-confirmed satisfaction or
			// a drop; record id under applied so caller telemetry
			// reflects the hint without double-stamping evidence.
			applied = append(applied, id)
			continue
		}
		row.Status = ACSatisfied
		row.SatisfiedAtIter = iter
		row.LastEvidence = "worker " + safeRole(role) + " handoff iter-" + itoa(iter) + " wave-" + safeWave(wave)
		applied = append(applied, id)
	}
	return applied, unknown
}

// ACSnapshot returns a fresh copy of state.AC suitable for callers
// that want to iterate without holding the mission lock (template
// renderers, status projectors, the approval-modal builder).
func (m *MissionState) ACSnapshot() []AcceptanceCriterion {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ac) == 0 {
		return nil
	}
	out := make([]AcceptanceCriterion, len(m.ac))
	copy(out, m.ac)
	return out
}

// HasUnsatisfiedAC reports whether the mission has any AC row whose
// status is neither satisfied nor dropped. The finish gate consults
// this — true => finish is gated (planner must address remaining
// rows); false => synthesis may run.
func (m *MissionState) HasUnsatisfiedAC() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.ac {
		if row.Status == ACUnsatisfied {
			return true
		}
	}
	return false
}

// UnsatisfiedAC returns the ids + statements of rows whose status is
// still unsatisfied. Used by the synthetic amend coercion in the
// planner loop's finish gate so the next planner sees exactly which
// rows still need work.
func (m *MissionState) UnsatisfiedAC() []AcceptanceCriterion {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AcceptanceCriterion
	for _, row := range m.ac {
		if row.Status == ACUnsatisfied {
			out = append(out, row)
		}
	}
	return out
}

// checkIDsLocked verifies every Update entry references an existing
// row. Returns the first failure. Caller holds m.mu.
func (m *MissionState) checkIDsLocked(updates []ACUpdateSpec) error {
	for i, u := range updates {
		id := strings.TrimSpace(u.ID)
		if id == "" {
			return fmt.Errorf("ac_update[%d]: id must be non-empty", i)
		}
		if m.findIDLocked(id) < 0 {
			return fmt.Errorf("ac_update[%d]: id %q does not match any existing acceptance criterion", i, id)
		}
	}
	return nil
}

// findIDLocked returns the slice index of the row with id, or -1
// when absent. Caller holds m.mu.
func (m *MissionState) findIDLocked(id string) int {
	id = strings.TrimSpace(id)
	if id == "" {
		return -1
	}
	for i := range m.ac {
		if m.ac[i].ID == id {
			return i
		}
	}
	return -1
}

// applyUpdateLocked mutates the row identified by u.ID per the
// update fields. Caller holds m.mu and has already validated id
// existence via checkIDsLocked.
func (m *MissionState) applyUpdateLocked(u ACUpdateSpec, iter int, evidenceSource string) error {
	idx := m.findIDLocked(u.ID)
	if idx < 0 {
		return fmt.Errorf("ac_update: id %q vanished between validation and apply", u.ID)
	}
	row := &m.ac[idx]
	if u.Statement != "" {
		stmt := strings.TrimSpace(u.Statement)
		if stmt == "" {
			return fmt.Errorf("ac_update[%s]: statement may not be whitespace-only", u.ID)
		}
		row.Statement = stmt
	}
	if u.Drop {
		row.Status = ACDropped
		row.DroppedAtIter = iter
		row.DropReason = strings.TrimSpace(u.DropReason)
		row.LastEvidence = "dropped: " + row.DropReason
		return nil
	}
	if u.Status != "" {
		row.Status = u.Status
		if u.Status == ACSatisfied && row.SatisfiedAtIter == 0 {
			row.SatisfiedAtIter = iter
		}
	}
	evidence := strings.TrimSpace(u.Evidence)
	if evidence == "" {
		evidence = evidenceSource
	}
	if evidence != "" {
		row.LastEvidence = evidence
	}
	return nil
}

// itoa is a small helper to avoid importing strconv just for one
// trivial conversion path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// safeRole / safeWave fall back to a placeholder when the worker
// handoff's role / wave string is empty.
func safeRole(role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		return "?"
	}
	return role
}

func safeWave(wave string) string {
	wave = strings.TrimSpace(wave)
	if wave == "" {
		return "?"
	}
	return wave
}
