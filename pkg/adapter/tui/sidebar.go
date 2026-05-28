package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// liveviewStatus is the TUI's typed view of the
// liveview/status frame's `Data`. Schema lives in
// pkg/extension/liveview/fold.go::emitStatus (kept in sync; if
// liveview adds a field it must stay additive — open Q #8 in the
// phase 5.1c spec).
type liveviewStatus struct {
	SessionID      string                      `json:"session_id"`
	Depth          int                         `json:"depth"`
	Tier           string                      `json:"tier,omitempty"` // Phase 6.1d — resolved tier from liveview emit; empty for legacy frames, fall back to shortTierLabel(Depth).
	LifecycleState string                      `json:"lifecycle_state,omitempty"`
	LastToolCall   *protocol.ToolCallRef       `json:"last_tool_call,omitempty"`
	PendingInquiry *protocol.PendingInquiryRef `json:"pending_inquiry,omitempty"`
	RecentActivity []protocol.ToolCallRef      `json:"recent_activity,omitempty"`
	RecentChildren []recentChildEntry          `json:"recent_children,omitempty"`
	ChildMeta      map[string]childMetaEntry   `json:"child_meta,omitempty"`
	Extensions     map[string]json.RawMessage  `json:"extensions,omitempty"`
	Children       map[string]*liveviewStatus  `json:"children,omitempty"`
	ContextBudget  *contextBudget              `json:"context_budget,omitempty"`
}

// contextBudget mirrors pkg/extension/liveview.ContextBudget on
// the TUI side. Phase 5.2 (context-budget ε) — adapter renders
// a dedicated sidebar pane summarising the session's prompt
// footprint.
type contextBudget struct {
	HistoryTokens int                  `json:"history_tokens,omitempty"`
	SessionUsage  *protocol.TokenUsage `json:"session_usage,omitempty"`
	ToolsTokens   int                  `json:"tools_tokens,omitempty"`
	Extensions    map[string]int       `json:"extensions,omitempty"`
	Skills        *skillsBudget        `json:"skills,omitempty"`
}

type skillsBudget struct {
	LoadedTokens    int `json:"loaded_tokens,omitempty"`
	AvailableTokens int `json:"available_tokens,omitempty"`
}

// childMetaEntry mirrors pkg/extension/liveview's childMetaEntry —
// spawn-time per-child metadata (role / skill) the parent
// captured from its own SubagentStarted emit. Surfaced alongside
// the children map so the tree can show role next to each node.
type childMetaEntry struct {
	Role      string    `json:"role,omitempty"`
	Skill     string    `json:"skill,omitempty"`
	Task      string    `json:"task,omitempty"`
	Tier      string    `json:"tier,omitempty"` // Phase 6.1d — captured from SubagentStartedPayload.Tier.
	StartedAt time.Time `json:"started_at,omitempty"`
}

// recentChildEntry mirrors pkg/extension/liveview's recentChild
// (same JSON shape). Carries enough info to render a "what just
// finished" timeline entry with a reason-coloured prefix.
type recentChildEntry struct {
	SessionID    string    `json:"session_id"`
	Depth        int       `json:"depth,omitempty"`
	Tier         string    `json:"tier,omitempty"` // Phase 6.1d — copied from the live child's status at termination time.
	Role         string    `json:"role,omitempty"`
	Skill        string    `json:"skill,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	LastTool     string    `json:"last_tool,omitempty"`
	TerminatedAt time.Time `json:"terminated_at"`
}

func parseLiveviewStatus(data json.RawMessage) (*liveviewStatus, error) {
	var s liveviewStatus
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// renderSidebar produces the right-pane sidebar string for the
// given liveview status frame. Width caps every line; height is
// the available vertical budget (used by lipgloss styling outside
// this function — internal text just appends as needed).
func renderSidebar(s *liveviewStatus, width int) string {
	if s == nil {
		return styleSidebarFaint.Render("waiting for liveview…")
	}
	if width <= 0 {
		width = 30
	}
	var sb strings.Builder

	// Tier header (root / mission / worker) + lifecycle pill.
	// Phase 6.1d — read the resolved tier when present, falling
	// back to depth-derive for legacy frames.
	sb.WriteString(styleSidebarHeading.Render("Tier: " + tierLabelOrDepth(s.Tier, s.Depth)))
	sb.WriteString("\n")
	sb.WriteString(lifecyclePill(s.LifecycleState))
	sb.WriteString("\n")

	// Pending inquiry — most actionable signal; render first
	// (after header) so the operator sees it without scrolling.
	if s.PendingInquiry != nil {
		sb.WriteString("\n")
		sb.WriteString(stylePendingInquiry.Render("⚠ inquiry pending"))
		sb.WriteString("\n")
		sb.WriteString(styleSidebarFaint.Render(truncate(s.PendingInquiry.Question, width-1)))
		sb.WriteString("\n")
	}

	// Active subagents — recursive subtree projection.
	if len(s.Children) > 0 {
		sb.WriteString("\n")
		sb.WriteString(styleSidebarHeading.Render("Subagents"))
		sb.WriteString("\n")
		for _, c := range sortedChildren(s.Children) {
			meta := s.ChildMeta[c.SessionID]
			sb.WriteString(renderSubagent(c, meta, 1, width))
		}
	}

	// Recently terminated subagents — history with reason badge.
	// Dogfood feedback: wave-based missions complete + re-spawn
	// constantly, which read like "the worker died" without this
	// trail. Reason badge differentiates legitimate completion
	// from cancellation / error.
	if len(s.RecentChildren) > 0 {
		sb.WriteString("\n")
		sb.WriteString(styleSidebarHeading.Render("Recent"))
		sb.WriteString("\n")
		for _, rc := range s.RecentChildren {
			sb.WriteString(renderRecentChild(rc, width))
		}
	}

	// Last tool — show what's running RIGHT NOW.
	if s.LastToolCall != nil {
		sb.WriteString("\n")
		sb.WriteString(styleSidebarHeading.Render("Last tool"))
		sb.WriteString("\n")
		sb.WriteString(truncate(s.LastToolCall.Name, width))
		sb.WriteString("\n")
		sb.WriteString(styleSidebarFaint.Render(ageString(s.LastToolCall.StartedAt)))
		sb.WriteString("\n")
	}

	// Plan — current step + progress hint.
	if plan := parsePlan(s.Extensions); plan != nil && plan.Active {
		sb.WriteString("\n")
		sb.WriteString(renderPlan(plan, width))
	}

	// Mission — PDCA progress for mission-tier sessions: active
	// wave, completed wave count with per-wave status badges, and
	// the handoff store size. Worker / planner / checker tiers see
	// nothing here (mission ext only emits this payload on mission
	// sessions).
	if mission := parseMissionStatus(s.Extensions); mission != nil {
		sb.WriteString("\n")
		sb.WriteString(renderMission(mission, width))
	}

	// Notepad — count + per-category breakdown.
	if buckets := parseNotepadCounts(s.Extensions); len(buckets) > 0 {
		sb.WriteString("\n")
		sb.WriteString(renderNotepad(buckets, width))
	}

	// Loaded skills — list names + tool count. Names are sorted
	// server-side by the skill extension; sidebar prints them
	// verbatim, one per line, truncated to width. Tool count
	// (when present in the payload) is shown next to the heading.
	if skills, toolsCount := parseSkillStatus(s.Extensions); len(skills) > 0 {
		sb.WriteString("\n")
		heading := fmt.Sprintf("Skills · %d", len(skills))
		if toolsCount > 0 {
			heading += fmt.Sprintf(" · %d tools", toolsCount)
		}
		sb.WriteString(styleSidebarHeading.Render(heading))
		sb.WriteString("\n")
		for _, name := range skills {
			sb.WriteString(styleSidebarFaint.Render(truncate("  "+name, width)))
			sb.WriteString("\n")
		}
	}

	// Context budget — current prompt-time footprint (history +
	// tools + extension blocks). Bottom of the sidebar so the
	// operator can scan it without it pushing actionable
	// signals (inquiry, subagents) off screen. Phase 5.2 ε.
	if cb := s.ContextBudget; cb != nil {
		sb.WriteString("\n")
		sb.WriteString(renderContextBudget(cb, width))
		// Lifetime usage is a different beast — accumulates
		// across every turn, so a long-running session sees it
		// dwarf the current-prompt numbers above. Rendered in
		// its own pane so the two don't read as comparable.
		if cb.SessionUsage != nil {
			sb.WriteString("\n")
			sb.WriteString(renderSessionUsage(cb.SessionUsage, width))
		}
	}

	return sb.String()
}

// renderSessionUsage prints the lifetime token spend pane.
// Separate from the context-budget pane because cumulative
// numbers (running total across every turn) are not directly
// comparable to current-prompt estimates — putting them
// alongside misleads the operator into thinking "session" is
// the current cost rather than the running total.
//
// Phase 5.2 ε.1 follow-up — dogfood feedback 2026-05-21.
func renderSessionUsage(u *protocol.TokenUsage, width int) string {
	var sb strings.Builder
	sb.WriteString(styleSidebarHeading.Render("Usage (lifetime)"))
	sb.WriteString("\n")
	sb.WriteString(styleSidebarFaint.Render(
		truncate(fmt.Sprintf("  prompt     %s tok", formatTokens(u.PromptTokens)), width)))
	sb.WriteString("\n")
	sb.WriteString(styleSidebarFaint.Render(
		truncate(fmt.Sprintf("  completion %s tok", formatTokens(u.CompletionTokens)), width)))
	sb.WriteString("\n")
	return sb.String()
}

// renderContextBudget produces the context-budget pane. Layout:
//
//	Context budget
//	  history       1.2k tok
//	  tools           240 tok
//	  compactor       800 tok
//	  notepad         400 tok
//	  skill (loaded)  1.5k tok
//	  skill (catalog)  600 tok
//	  ─────────
//	  session usage  45.0k → 8.0k tok
//
// Every line is omitempty — zero / nil dimensions are skipped so
// fresh sessions show only what's actually populated.
func renderContextBudget(b *contextBudget, width int) string {
	var sb strings.Builder
	sb.WriteString(styleSidebarHeading.Render("Context budget"))
	sb.WriteString("\n")
	if b.HistoryTokens > 0 {
		sb.WriteString(styleSidebarFaint.Render(
			truncate(fmt.Sprintf("  history    %s tok", formatTokens(b.HistoryTokens)), width)))
		sb.WriteString("\n")
	}
	if b.ToolsTokens > 0 {
		sb.WriteString(styleSidebarFaint.Render(
			truncate(fmt.Sprintf("  tools      %s tok", formatTokens(b.ToolsTokens)), width)))
		sb.WriteString("\n")
	}
	// Stable extension order — name-sorted so the pane doesn't
	// reshuffle between status emits.
	if len(b.Extensions) > 0 {
		names := make([]string, 0, len(b.Extensions))
		for n := range b.Extensions {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			v := b.Extensions[n]
			if v == 0 {
				continue
			}
			sb.WriteString(styleSidebarFaint.Render(
				truncate(fmt.Sprintf("  %s   %s tok", n, formatTokens(v)), width)))
			sb.WriteString("\n")
		}
	}
	if b.Skills != nil {
		if b.Skills.LoadedTokens > 0 {
			sb.WriteString(styleSidebarFaint.Render(
				truncate(fmt.Sprintf("  skill (loaded)  %s tok",
					formatTokens(b.Skills.LoadedTokens)), width)))
			sb.WriteString("\n")
		}
		if b.Skills.AvailableTokens > 0 {
			sb.WriteString(styleSidebarFaint.Render(
				truncate(fmt.Sprintf("  skill (catalog) %s tok",
					formatTokens(b.Skills.AvailableTokens)), width)))
			sb.WriteString("\n")
		}
	}
	// SessionUsage moved to its own renderSessionUsage pane —
	// lifetime numbers don't belong next to per-prompt
	// estimates. See sidebar.go renderSessionUsage call site.
	return sb.String()
}

// formatTokens renders an integer token count with k / M suffix
// when large so the sidebar pane stays narrow. Rules:
//
//	    0 →     "0"
//	  500 →   "500"
//	 1500 →  "1.5k"
//	45000 → "45.0k"
//	2000000 → "2.0M"
func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}

// renderSubagent prints one subagent node with `indent` levels of
// indent. Walks Children recursively so a worker under a mission
// under root shows up as `▸ mission` then `  ▸ worker`. When
// RecentActivity is present (phase 5.1c S1) the last 2–3 tool
// calls are rendered as a stripe under the node — gives the
// operator a "what is this subagent doing right now" hint
// beyond the single LastToolCall.
//
// `meta` carries the spawn-time role / skill captured by the
// parent's liveview. When present, the node label includes
// "<tier>:<role>" so the operator sees which staged role
// (schema-explorer / query-builder / …) is currently running.
func renderSubagent(s *liveviewStatus, meta childMetaEntry, indent, width int) string {
	if s == nil {
		return ""
	}
	prefix := strings.Repeat("  ", indent-1) + "▸ "
	// Phase 6.1d — prefer the resolved tier carried in the child's
	// status (or in the parent's spawn-time meta), falling back to
	// depth-derive only for legacy frames lacking the field.
	tier := s.Tier
	if tier == "" {
		tier = meta.Tier
	}
	label := tierLabelOrDepth(tier, s.Depth)
	if meta.Role != "" {
		label = label + ":" + meta.Role
	}
	state := s.LifecycleState
	if state == "" {
		state = "active"
	}
	line := fmt.Sprintf("%s%s · %s", prefix, label, state)
	out := truncate(line, width) + "\n"
	// Prefer the rolling recent_activity window (3 tools) over
	// the single last_tool_call when present — gives the operator
	// recent context, not just "what's running right now".
	indentPrefix := strings.Repeat("  ", indent)
	if len(s.RecentActivity) > 0 {
		for i, ref := range s.RecentActivity {
			marker := " "
			if i == 0 {
				marker = "▸" // most recent gets a leader
			}
			toolLine := fmt.Sprintf("%s%s %s · %s",
				indentPrefix, marker, ref.Name, ageString(ref.StartedAt))
			out += styleSidebarFaint.Render(truncate(toolLine, width)) + "\n"
		}
	} else if s.LastToolCall != nil {
		toolLine := indentPrefix + s.LastToolCall.Name
		out += styleSidebarFaint.Render(truncate(toolLine, width)) + "\n"
	}
	for _, c := range sortedChildren(s.Children) {
		// Pull role/skill from THIS node's own ChildMeta — it
		// carries the spawn-time metadata for its direct children.
		out += renderSubagent(c, s.ChildMeta[c.SessionID], indent+1, width)
	}
	return out
}

// planSnapshot mirrors plan.Plan as marshalled (no json tags →
// exported field names). Decoded out of
// liveviewStatus.Extensions["plan"].
type planSnapshot struct {
	Active      bool      `json:"Active"`
	Text        string    `json:"Text"`
	CurrentStep string    `json:"CurrentStep"`
	Comments    []any     `json:"Comments"`
	SetAt       time.Time `json:"SetAt"`
	UpdatedAt   time.Time `json:"UpdatedAt"`
}

func parsePlan(exts map[string]json.RawMessage) *planSnapshot {
	raw, ok := exts["plan"]
	if !ok {
		return nil
	}
	var p planSnapshot
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	return &p
}

func renderPlan(p *planSnapshot, width int) string {
	var sb strings.Builder
	sb.WriteString(styleSidebarHeading.Render("Plan"))
	sb.WriteString("\n")
	if step := strings.TrimSpace(p.CurrentStep); step != "" {
		sb.WriteString("→ " + truncate(step, width-2))
		sb.WriteString("\n")
	}
	if len(p.Comments) > 0 {
		sb.WriteString(styleSidebarFaint.Render(fmt.Sprintf("%d comments", len(p.Comments))))
		sb.WriteString("\n")
	}
	return sb.String()
}

// notepadStatus mirrors pkg/extension/notepad's ReportStatus wire
// shape (phase 5.1c). `counts` holds server-side bucket totals —
// authoritative for sidebar display; `recent` is kept for a future
// "recent notes" panel.
type notepadStatus struct {
	Counts map[string]int `json:"counts"`
	Recent []struct {
		ID       string `json:"id"`
		Category string `json:"category,omitempty"`
	} `json:"recent"`
}

// parseNotepadCounts returns sorted (category, count) pairs from
// the liveview notepad payload. Reads `counts` directly — no
// derivation from the truncated `recent` list.
func parseNotepadCounts(exts map[string]json.RawMessage) []categoryCount {
	raw, ok := exts["notepad"]
	if !ok {
		return nil
	}
	var s notepadStatus
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	if len(s.Counts) == 0 {
		return nil
	}
	out := make([]categoryCount, 0, len(s.Counts))
	for cat, n := range s.Counts {
		label := cat
		if label == "" {
			label = "uncategorized"
		}
		out = append(out, categoryCount{Category: label, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Category < out[j].Category
	})
	return out
}

type categoryCount struct {
	Category string
	Count    int
}

func renderNotepad(buckets []categoryCount, width int) string {
	var sb strings.Builder
	total := 0
	for _, b := range buckets {
		total += b.Count
	}
	sb.WriteString(styleSidebarHeading.Render(fmt.Sprintf("Notepad · %d", total)))
	sb.WriteString("\n")
	for _, b := range buckets {
		line := fmt.Sprintf("  %s (%d)", b.Category, b.Count)
		sb.WriteString(truncate(line, width))
		sb.WriteString("\n")
	}
	return sb.String()
}

// missionStatus mirrors pkg/extension/mission.ReportStatus's wire
// shape. Used by renderMission to surface PDCA progress (current
// wave, completed wave statuses, handoff store size) in the
// sidebar's mission section. Only populated on mission-tier
// sessions; root / worker / planner / checker tiers parse nil.
type missionStatus struct {
	Plan struct {
		Done []struct {
			Label     string `json:"label"`
			Status    string `json:"status"`
			Subagents []struct {
				Name   string `json:"name"`
				Role   string `json:"role,omitempty"`
				Status string `json:"status,omitempty"`
			} `json:"subagents,omitempty"`
		} `json:"done"`
	} `json:"plan"`
	ActiveWave   string `json:"active_wave,omitempty"`
	HandoffCount int    `json:"handoff_count"`
}

// parseMissionStatus extracts the mission payload from a status
// frame's extensions map. Returns nil when the session is not a
// mission (mission ext only contributes on mission-tier sessions).
func parseMissionStatus(exts map[string]json.RawMessage) *missionStatus {
	raw, ok := exts["mission"]
	if !ok {
		return nil
	}
	var s missionStatus
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	if s.ActiveWave == "" && len(s.Plan.Done) == 0 && s.HandoffCount == 0 {
		return nil
	}
	return &s
}

// renderMission prints the PDCA progress block. Internal waves
// (label starts with `_`, e.g. `_plan-1`, `_check-1`, `_synth`)
// stay folded behind a counter so the operator sees the Do-waves
// as the primary timeline.
func renderMission(m *missionStatus, width int) string {
	var sb strings.Builder
	heading := fmt.Sprintf("Mission · %d handoffs", m.HandoffCount)
	sb.WriteString(styleSidebarHeading.Render(heading))
	sb.WriteString("\n")
	if m.ActiveWave != "" {
		line := "→ " + m.ActiveWave
		if strings.HasPrefix(m.ActiveWave, "_") {
			line = "→ " + m.ActiveWave + " (internal)"
		}
		sb.WriteString(truncate(line, width))
		sb.WriteString("\n")
	}
	internal := 0
	for _, w := range m.Plan.Done {
		if strings.HasPrefix(w.Label, "_") {
			internal++
			continue
		}
		badge := "✓"
		if w.Status == "failed" {
			badge = "✗"
		} else if w.Status != "ok" && w.Status != "" {
			badge = "·"
		}
		line := fmt.Sprintf("  %s %s", badge, w.Label)
		sb.WriteString(styleSidebarFaint.Render(truncate(line, width)))
		sb.WriteString("\n")
	}
	if internal > 0 {
		line := fmt.Sprintf("  · %d planner/checker waves", internal)
		sb.WriteString(styleSidebarFaint.Render(truncate(line, width)))
		sb.WriteString("\n")
	}
	return sb.String()
}

// parseSkillStatus pulls the skill extension's liveview payload
// (loaded skill names + total tool count). Phase 5.1c followup —
// the `tools` field is optional; consumers that only see the
// 5.1b shape get tools=0 and render just the skill list.
func parseSkillStatus(exts map[string]json.RawMessage) (skills []string, tools int) {
	raw, ok := exts["skill"]
	if !ok {
		return nil, 0
	}
	var s struct {
		Loaded []string `json:"loaded"`
		Tools  int      `json:"tools"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, 0
	}
	return s.Loaded, s.Tools
}

// renderRecentChild prints one history entry as a two-line block:
//
//	● mission · completed · 12s ago
//	  last: hugr.execute_query
//
// Where the leading dot's colour encodes the reason category.
// Phase 5.1c dogfood follow-up.
func renderRecentChild(rc recentChildEntry, width int) string {
	var sb strings.Builder
	label := tierLabelOrDepth(rc.Tier, rc.Depth)
	if rc.Role != "" {
		label = label + ":" + rc.Role
	}
	reason := rc.Reason
	if reason == "" {
		reason = "done"
	}
	style := reasonStyle(reason)
	line := fmt.Sprintf("%s %s · %s · %s",
		style.Render("●"), label, reasonShort(reason), ageString(rc.TerminatedAt))
	sb.WriteString(truncate(line, width))
	sb.WriteString("\n")
	if rc.LastTool != "" {
		sb.WriteString(styleSidebarFaint.Render(truncate("  last: "+rc.LastTool, width)))
		sb.WriteString("\n")
	}
	return sb.String()
}

// reasonStyle picks the dot colour by reason category. Mirrors
// the spec's success / mid / failure tiers without enumerating
// every possible reason string.
func reasonStyle(reason string) lipgloss.Style {
	switch {
	case reason == "completed":
		return styleSidebarActive // green
	case strings.HasPrefix(reason, "cancel"), reason == "timeout":
		return stylePendingInquiry // amber
	case strings.HasPrefix(reason, "error"), reason == "abnormal_close":
		return lipgloss.NewStyle().Foreground(activeTheme.ErrorFG)
	default:
		return styleSidebarFaint
	}
}

// reasonShort trims and compresses reason for the one-line slot.
// "error: stream_error" → "stream_error"; long reasons truncate
// inside the caller's width budget.
func reasonShort(reason string) string {
	const errPrefix = "error: "
	if strings.HasPrefix(reason, errPrefix) {
		return reason[len(errPrefix):]
	}
	return reason
}

func sortedChildren(m map[string]*liveviewStatus) []*liveviewStatus {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*liveviewStatus, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func tierLabel(depth int) string {
	return "Tier: " + shortTierLabel(depth)
}

// shortTierLabel is the indent-friendly version used inside the
// subtree stripe; the top-level sidebar header uses tierLabel for
// the explicit "Tier: " prefix.
//
// Phase 6.1d: tier-aware spawn lets a child's semantic tier diverge
// from its structural depth (e.g. an ad-hoc recipe child at
// depth=1 carries worker semantics). When the caller has the
// resolved tier value it should pass it via [tierLabelOrDepth];
// this helper is the legacy depth-only fallback for events whose
// upstream did not populate Tier (rows persisted before this
// commit, frame payloads predating the protocol field).
func shortTierLabel(depth int) string {
	switch depth {
	case 0:
		return "root"
	case 1:
		return "mission"
	default:
		return "worker"
	}
}

// tierLabelOrDepth returns the resolved tier value when non-empty,
// otherwise falls back to depth-derived [shortTierLabel]. Used by
// every sidebar / mission-modal renderer that has access to a
// liveview status or child meta carrying the Tier field. Phase
// 6.1d.
func tierLabelOrDepth(tier string, depth int) string {
	if tier != "" {
		return tier
	}
	return shortTierLabel(depth)
}

func lifecyclePill(state string) string {
	if state == "" {
		state = "idle"
	}
	style := styleSidebarFaint
	switch state {
	case "wait_approval", "wait_user_input":
		style = stylePendingInquiry
	case "wait_subagents", "active":
		style = styleSidebarActive
	}
	return style.Render("● " + state)
}

func ageString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func truncate(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	return s[:width-1] + "…"
}

var (
	styleSidebarHeading = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleSidebarFaint   = lipgloss.NewStyle().Faint(true)
	styleSidebarActive  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	stylePendingInquiry = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
)
