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
	LifecycleState string                      `json:"lifecycle_state,omitempty"`
	LastToolCall   *protocol.ToolCallRef       `json:"last_tool_call,omitempty"`
	PendingInquiry *protocol.PendingInquiryRef `json:"pending_inquiry,omitempty"`
	RecentActivity []protocol.ToolCallRef      `json:"recent_activity,omitempty"`
	Extensions     map[string]json.RawMessage  `json:"extensions,omitempty"`
	Children       map[string]*liveviewStatus  `json:"children,omitempty"`
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
	sb.WriteString(styleSidebarHeading.Render(tierLabel(s.Depth)))
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
			sb.WriteString(renderSubagent(c, 1, width))
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

	return sb.String()
}

// renderSubagent prints one subagent node with `indent` levels of
// indent. Walks Children recursively so a worker under a mission
// under root shows up as `▸ mission` then `  ▸ worker`. When
// RecentActivity is present (phase 5.1c S1) the last 2–3 tool
// calls are rendered as a stripe under the node — gives the
// operator a "what is this subagent doing right now" hint
// beyond the single LastToolCall.
func renderSubagent(s *liveviewStatus, indent, width int) string {
	if s == nil {
		return ""
	}
	prefix := strings.Repeat("  ", indent-1) + "▸ "
	label := shortTierLabel(s.Depth)
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
		out += renderSubagent(c, indent+1, width)
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
