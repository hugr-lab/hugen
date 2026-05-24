package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// inquiryState is the TUI's view of a live [protocol.InquiryRequest]
// while waiting for the operator's answer. Set when the frame lands,
// cleared on the echo [protocol.InquiryResponse] or on
// [protocol.SessionTerminated]. Slice 3 — modal overlay UX per
// phase-5.1c §7.
type inquiryState struct {
	req protocol.InquiryRequestPayload
	// callerLabel is a short "from <session>" hint shown in the
	// modal title. Resolved from req.CallerSessionID; empty when
	// the inquiry originates from root.
	callerLabel string
	// replyMode flips true when the operator presses `r` on an
	// approval inquiry; the textarea below the modal is focused and
	// the next Enter submits "/approve <reason>" or "/deny <reason>".
	// For clarifications replyMode is always true (the textarea is
	// the only input path).
	replyMode bool
	// replyVerb is the approval verdict the typed reason will be
	// stitched onto: "approve" or "deny". Empty for clarifications.
	replyVerb string

	// Phase 5.x — B15. Tab-style batched-research state. The
	// modal walks the operator through clarifications one at a
	// time, with three panels per question (value / comment /
	// review). State:
	//   - `currentIdx` names the clarification being asked.
	//     `len(req.Clarifications)` is a special "review" slot
	//     where the operator audits all answers before
	//     submitting.
	//   - `panel` selects the per-question panel: 0=value,
	//     1=comment, 2=review (review-of-current-question).
	//     For comment-kind clarifications panel is forced to 1
	//     (no value field rendered).
	//   - `optionHighlights` caches each option-bearing
	//     question's cursor index — what ↑/↓ moved to. Persists
	//     across navigation so a back-walk doesn't lose the
	//     prior pick. `-1` means "no option highlighted yet".
	//   - `optionPicks` caches the multi-select set per
	//     question id (sorted index list). `nil` means
	//     single-select.
	//   - `answers` accumulates per-id entries as the operator
	//     advances. Built into payload.Answers on submit.
	currentIdx       int
	panel            int
	optionHighlights map[string]int
	optionPicks      map[string]map[int]struct{}
	answers          map[string]protocol.AnswerEntry
}

// Per-question panel identifiers.
const (
	batchedPanelValue   = 0
	batchedPanelComment = 1
	batchedPanelReview  = 2
)

// isBatched reports whether this state is rendering a research_batch
// modal. Used to switch the modal renderer + key dispatch into the
// one-question-at-a-time tab walk.
func (s *inquiryState) isBatched() bool {
	return s != nil && s.req.Type == protocol.InquiryTypeResearchBatch && len(s.req.Clarifications) > 0
}

// currentClarification returns the clarification the operator is
// currently answering.
//
// Precondition: caller has verified isBatched()==true AND
// onReview()==false. The zero-value fallback on out-of-bounds
// (review screen or a corrupt currentIdx) is intentional — it
// keeps the renderer from crashing on a malformed state, but the
// returned Clarification will have an empty ID/Question and option
// helpers (togglePickedOption / pickedOptionsValue /
// optionIsPicked) will bail on the empty-options guard. Callers
// that depend on a real question MUST gate their work behind the
// preconditions above; do NOT use this on the review screen.
func (s *inquiryState) currentClarification() protocol.Clarification {
	if s.currentIdx < 0 || s.currentIdx >= len(s.req.Clarifications) {
		return protocol.Clarification{}
	}
	return s.req.Clarifications[s.currentIdx]
}

// onReview reports whether the modal cursor sits on the final
// review-and-submit screen rather than a per-question screen.
// currentIdx == len(req.Clarifications) is the canonical "review"
// position the operator reaches after Enter on the last question
// (or via right-arrow nav).
func (s *inquiryState) onReview() bool {
	return s.isBatched() && s.currentIdx >= len(s.req.Clarifications)
}

// togglePickedOption flips the membership of the highlighted option
// in the current question's multi-select set. No-op when the
// question doesn't declare multi=true or no option is highlighted.
func (s *inquiryState) togglePickedOption() {
	if !s.isBatched() {
		return
	}
	cur := s.currentClarification()
	if !cur.Multi || len(cur.Options) == 0 {
		return
	}
	hi := s.optionHighlight()
	if hi < 0 || hi >= len(cur.Options) {
		return
	}
	if s.optionPicks == nil {
		s.optionPicks = make(map[string]map[int]struct{})
	}
	set, ok := s.optionPicks[cur.ID]
	if !ok {
		set = make(map[int]struct{})
		s.optionPicks[cur.ID] = set
	}
	if _, present := set[hi]; present {
		delete(set, hi)
	} else {
		set[hi] = struct{}{}
	}
}

// pickedOptionsValue returns the comma-joined option texts for the
// current question's multi-select picks (sorted by index). Empty
// string when no picks. Used as the Value when committing a
// multi-select question.
func (s *inquiryState) pickedOptionsValue() string {
	if s.optionPicks == nil {
		return ""
	}
	cur := s.currentClarification()
	set, ok := s.optionPicks[cur.ID]
	if !ok || len(set) == 0 {
		return ""
	}
	indices := make([]int, 0, len(set))
	for i := range set {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	parts := make([]string, 0, len(indices))
	for _, i := range indices {
		if i >= 0 && i < len(cur.Options) {
			parts = append(parts, cur.Options[i])
		}
	}
	return strings.Join(parts, ", ")
}

// optionIsPicked reports whether the option at idx is in the
// current question's multi-select set. Used by the renderer to
// draw the [x] marker.
func (s *inquiryState) optionIsPicked(idx int) bool {
	if s.optionPicks == nil {
		return false
	}
	cur := s.currentClarification()
	set, ok := s.optionPicks[cur.ID]
	if !ok {
		return false
	}
	_, present := set[idx]
	return present
}

// optionHighlight returns the cached option index for the current
// clarification (-1 when the operator hasn't pressed ↑/↓ on this
// question yet). Used by the renderer to mark the highlighted row
// and by the commit path to use the highlight as the value when
// the operator left the value textarea empty.
func (s *inquiryState) optionHighlight() int {
	if s.optionHighlights == nil {
		return -1
	}
	idx, ok := s.optionHighlights[s.currentClarification().ID]
	if !ok {
		return -1
	}
	return idx
}

// setOptionHighlight caches the cursor index for the current
// clarification.
func (s *inquiryState) setOptionHighlight(idx int) {
	if s.optionHighlights == nil {
		s.optionHighlights = make(map[string]int)
	}
	s.optionHighlights[s.currentClarification().ID] = idx
}

// cycleOptionHighlight advances (delta=+1) or rolls back (delta=-1)
// the current question's option highlight, wrapping at the bounds.
// No-op when the question has no options.
func (s *inquiryState) cycleOptionHighlight(delta int) {
	cur := s.currentClarification()
	if len(cur.Options) == 0 {
		return
	}
	idx := s.optionHighlight()
	if idx < 0 {
		// First ↑/↓ press selects the first option for ↓, last for
		// ↑ — matches list-widget conventions.
		if delta > 0 {
			idx = 0
		} else {
			idx = len(cur.Options) - 1
		}
		s.setOptionHighlight(idx)
		return
	}
	idx = (idx + delta + len(cur.Options)) % len(cur.Options)
	s.setOptionHighlight(idx)
}

// resolveOptionPick returns (matched-option-text, true) when `text`
// is a digit selecting one of the current clarification's options
// (`1` → options[0], etc.). Returns ("", false) otherwise so the
// caller treats the raw text as the answer. Kept as a power-user
// shortcut alongside the ↑/↓ + Enter flow.
func (s *inquiryState) resolveOptionPick(text string) (string, bool) {
	if !s.isBatched() || s.panel == batchedPanelComment {
		return "", false
	}
	cur := s.currentClarification()
	if len(cur.Options) == 0 {
		return "", false
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	if t[0] < '1' || t[0] > '9' {
		return "", false
	}
	idx := int(t[0]-'0') - 1
	if idx < 0 || idx >= len(cur.Options) {
		return "", false
	}
	rest := strings.TrimSpace(t[1:])
	if rest != "" && rest != "." {
		return "", false
	}
	return cur.Options[idx], true
}

// newInquiryState builds the modal handle from an inbound request
// frame. Auto-enters reply mode for clarifications + research_batch
// since they have no keystroke shortcuts.
func newInquiryState(req *protocol.InquiryRequest) *inquiryState {
	s := &inquiryState{
		req:         req.Payload,
		callerLabel: shortID(req.Payload.CallerSessionID),
	}
	switch req.Payload.Type {
	case protocol.InquiryTypeClarification, protocol.InquiryTypeResearchBatch:
		s.replyMode = true
	}
	// Phase 5.x — B15. Initialise focus for the batched walk:
	// comment-kind first question locks focus on the comment
	// field; everyone else starts on the value field.
	if s.isBatched() {
		if s.currentClarification().Kind == protocol.ClarificationKindComment {
			s.panel = batchedPanelComment
		} else {
			s.panel = batchedPanelValue
		}
	}
	return s
}

// title produces the modal header.
func (s *inquiryState) title() string {
	switch s.req.Type {
	case protocol.InquiryTypeApproval:
		if s.callerLabel != "" {
			return "Approval required (from " + s.callerLabel + ")"
		}
		return "Approval required"
	case protocol.InquiryTypeClarification:
		if s.callerLabel != "" {
			return "Clarification needed (from " + s.callerLabel + ")"
		}
		return "Clarification needed"
	case protocol.InquiryTypeResearchBatch:
		if s.callerLabel != "" {
			return "Research clarifications (from " + s.callerLabel + ")"
		}
		return "Research clarifications"
	default:
		return "Inquiry: " + s.req.Type
	}
}

// renderInquiryModal returns the rendered modal block sized to
// width. Height is determined by content; callers JoinVertical the
// result over chat. Caller must guarantee state != nil.
func renderInquiryModal(state *inquiryState, width int) string {
	if width < 30 {
		width = 30
	}
	contentW := width - 4 // border + padding
	if contentW < 10 {
		contentW = 10
	}

	// Tab-style batched flow: one clarification at a time, with
	// per-question progress indicator + numbered options pick-list
	// + per-question comment phase (toggled via Tab). Phase 5.x
	// — B15.
	if state.isBatched() {
		return renderBatchedInquiryModal(state, width, contentW)
	}

	var sb strings.Builder
	sb.WriteString(inquiryTitleStyle.Render(state.title()))
	sb.WriteString("\n\n")
	if q := strings.TrimSpace(state.req.Question); q != "" {
		sb.WriteString(wrap(q, contentW))
		sb.WriteString("\n")
	}
	if c := strings.TrimSpace(state.req.Context); c != "" {
		sb.WriteString("\n")
		sb.WriteString(inquiryFaintStyle.Render(wrap("Context: "+c, contentW)))
		sb.WriteString("\n")
	}
	if len(state.req.Options) > 0 {
		sb.WriteString("\n")
		sb.WriteString(inquiryFaintStyle.Render("Options:"))
		sb.WriteString("\n")
		for _, opt := range state.req.Options {
			sb.WriteString("  - ")
			sb.WriteString(truncate(opt, contentW-4))
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
	sb.WriteString(inquiryHintStyle.Render(actionHint(state)))

	return inquiryBoxStyle.Width(width - 2).Render(sb.String())
}

// renderBatchedInquiryModal renders the modal for research_batch
// inquiries. Phase 5.x — B15 + follow-up UX. Each question is its
// own screen with two panels (value + comment); a final "review"
// screen at currentIdx == N renders the accumulated answers + a
// Submit hint. The top of the modal carries a panel tab-bar
// showing which panel has focus.
func renderBatchedInquiryModal(state *inquiryState, width, contentW int) string {
	total := len(state.req.Clarifications)

	var sb strings.Builder
	title := batchedTitle(state, total)
	sb.WriteString(inquiryTitleStyle.Render(title))
	sb.WriteString("\n")
	sb.WriteString(renderBatchedTabBar(state))
	sb.WriteString("\n\n")

	if c := strings.TrimSpace(state.req.Context); c != "" {
		sb.WriteString(inquiryFaintStyle.Render(wrap("Context: "+c, contentW)))
		sb.WriteString("\n\n")
	}

	if state.onReview() {
		renderBatchedReviewBody(&sb, state, contentW)
	} else {
		renderBatchedQuestionBody(&sb, state, contentW)
	}

	sb.WriteString("\n")
	sb.WriteString(inquiryHintStyle.Render(batchedHint(state)))
	return inquiryBoxStyle.Width(width - 2).Render(sb.String())
}

// batchedTitle produces the modal header line. On per-question
// screens shows "[N/total]"; on the review screen shows "[review]".
func batchedTitle(state *inquiryState, total int) string {
	prefix := "Research clarifications"
	tail := ""
	if state.callerLabel != "" {
		tail = " (from " + state.callerLabel + ")"
	}
	if state.onReview() {
		return prefix + " [review]" + tail
	}
	return fmt.Sprintf("%s [%d/%d]%s", prefix, state.currentIdx+1, total, tail)
}

// renderBatchedTabBar draws the panel selector at the top of the
// modal: "[●value] [comment] [review]" where the active panel has
// the filled bullet. On the global review screen the "review" tab
// is the only one that highlights; per-question screens highlight
// value or comment based on state.panel.
func renderBatchedTabBar(state *inquiryState) string {
	labels := []struct {
		name   string
		active bool
	}{
		{"value", !state.onReview() && state.panel == batchedPanelValue},
		{"comment", !state.onReview() && state.panel == batchedPanelComment},
		{"review", state.onReview()},
	}
	var sb strings.Builder
	for i, l := range labels {
		if i > 0 {
			sb.WriteString(" ")
		}
		marker := "○"
		styled := inquiryFaintStyle
		if l.active {
			marker = "●"
			styled = inquiryHintStyle
		}
		sb.WriteString(styled.Render(fmt.Sprintf("[%s %s]", marker, l.name)))
	}
	return sb.String()
}

// renderBatchedQuestionBody is the per-question content panel.
// Layout: kind label + question text + option list (with ↑↓
// highlight + multi-select checkboxes when applicable) + the
// inline value/comment summary rows.
func renderBatchedQuestionBody(sb *strings.Builder, state *inquiryState, contentW int) {
	cur := state.currentClarification()
	commentKind := cur.Kind == protocol.ClarificationKindComment

	label := fmt.Sprintf("%s (%s)", cur.ID, kindOrDefault(cur.Kind))
	if cur.Multi && len(cur.Options) > 0 {
		label += " · multi-select"
	}
	sb.WriteString(inquiryFaintStyle.Render(label))
	sb.WriteString("\n")
	sb.WriteString(wrap(cur.Question, contentW))
	sb.WriteString("\n")

	if len(cur.Options) > 0 {
		sb.WriteString("\n")
		hint := "options (↑↓ to pick, Enter on value to commit)"
		if cur.Multi {
			hint = "options (↑↓ to navigate, Space to toggle, Enter on value to commit)"
		}
		sb.WriteString(inquiryFaintStyle.Render(hint + ":"))
		sb.WriteString("\n")
		hi := state.optionHighlight()
		for i, opt := range cur.Options {
			marker := "  "
			if i == hi {
				marker = "▸ "
			}
			box := ""
			if cur.Multi {
				if state.optionIsPicked(i) {
					box = "[x] "
				} else {
					box = "[ ] "
				}
			}
			line := fmt.Sprintf("%s%d. %s%s", marker, i+1, box, truncate(opt, contentW-10))
			if i == hi {
				sb.WriteString(inquiryHintStyle.Render(line))
			} else {
				sb.WriteString(line)
			}
			sb.WriteString("\n")
		}
	}
	if cur.Default != "" {
		sb.WriteString(inquiryFaintStyle.Render(wrap("  default: "+cur.Default, contentW)))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	entry := state.answers[cur.ID]
	if !commentKind {
		sb.WriteString(renderBatchedField(
			"value",
			state.panel == batchedPanelValue,
			fieldValuePreview(state, cur, entry),
			contentW,
		))
		sb.WriteString("\n")
	}
	sb.WriteString(renderBatchedField(
		"comment",
		state.panel == batchedPanelComment,
		fieldCommentPreview(state, cur, entry),
		contentW,
	))
	sb.WriteString("\n")
}

// renderBatchedReviewBody is the global pre-submit summary screen.
// Lists every clarification with its captured value + comment so
// the operator can audit before Enter. Renders "(no answer)" /
// "(no comment)" placeholders so missing entries are obvious.
func renderBatchedReviewBody(sb *strings.Builder, state *inquiryState, contentW int) {
	sb.WriteString(inquiryFaintStyle.Render("Review your answers — Enter submits, ← back to edit any question."))
	sb.WriteString("\n\n")
	for i, c := range state.req.Clarifications {
		entry := state.answers[c.ID]
		header := fmt.Sprintf("q%d · %s (%s)", i+1, c.ID, kindOrDefault(c.Kind))
		if c.Multi && len(c.Options) > 0 {
			header += " · multi-select"
		}
		sb.WriteString(inquiryHintStyle.Render(header))
		sb.WriteString("\n")
		sb.WriteString(inquiryFaintStyle.Render(wrap("  "+c.Question, contentW)))
		sb.WriteString("\n")
		if c.Kind != protocol.ClarificationKindComment {
			val := entry.Value
			if val == "" {
				val = inquiryFaintStyle.Render("(no answer)")
			}
			sb.WriteString(wrap("  value:   "+val, contentW))
			sb.WriteString("\n")
		}
		cmt := entry.Comment
		if cmt == "" {
			cmt = inquiryFaintStyle.Render("(no comment)")
		}
		sb.WriteString(wrap("  comment: "+cmt, contentW))
		sb.WriteString("\n\n")
	}
}

// renderBatchedField renders one labelled field row. The focused
// row gets a `>` marker + bright color; unfocused rows render
// faint to keep the eye on the active input.
func renderBatchedField(label string, focused bool, preview string, contentW int) string {
	marker := "  "
	style := inquiryFaintStyle
	if focused {
		marker = "> "
		style = inquiryHintStyle
	}
	line := fmt.Sprintf("%s%s: %s", marker, label, truncate(preview, contentW-len(label)-4))
	return style.Render(line)
}

// fieldValuePreview returns the inline preview text for the value
// row. When the value field has focus, show what the operator is
// currently typing (mirrored from the real textarea by the caller —
// we receive it through the state's last-known content). Otherwise
// show the committed value, the highlighted option, or "—".
//
// Today we don't mirror the live textarea content into state on
// every keystroke (the textarea owns its own buffer); the preview
// falls back to the persisted entry. The hint footer still makes
// it clear which field is active.
func fieldValuePreview(state *inquiryState, cur protocol.Clarification, entry protocol.AnswerEntry) string {
	if entry.Value != "" {
		return entry.Value
	}
	if hi := state.optionHighlight(); hi >= 0 && hi < len(cur.Options) {
		return inquiryFaintStyle.Render("(↑↓ highlighted: " + cur.Options[hi] + ")")
	}
	if state.panel == batchedPanelValue {
		return inquiryFaintStyle.Render("(type to fill)")
	}
	return inquiryFaintStyle.Render("—")
}

// fieldCommentPreview mirrors fieldValuePreview for the comment row.
func fieldCommentPreview(state *inquiryState, _ protocol.Clarification, entry protocol.AnswerEntry) string {
	if entry.Comment != "" {
		return entry.Comment
	}
	if state.panel == batchedPanelComment {
		return inquiryFaintStyle.Render("(type to fill)")
	}
	return inquiryFaintStyle.Render("—")
}

// batchedHint is the one-line keystroke summary at the bottom of
// the batched modal. Renders different hints based on the current
// panel + screen (per-question vs review).
func batchedHint(state *inquiryState) string {
	if state.onReview() {
		return "[← prev question | Enter submit batch | Esc abort]"
	}
	cur := state.currentClarification()
	parts := []string{}
	// Per-panel cues
	if state.panel == batchedPanelValue && !state.onReview() && cur.Kind != protocol.ClarificationKindComment {
		if len(cur.Options) > 0 {
			if cur.Multi {
				parts = append(parts, "↑↓ navigate option", "Space toggle pick")
			} else {
				parts = append(parts, "↑↓ pick option")
			}
		}
	}
	// Panel switch
	parts = append(parts, "Tab next panel")
	// Question nav
	if state.currentIdx > 0 {
		parts = append(parts, "← prev q")
	}
	if state.currentIdx < len(state.req.Clarifications) {
		parts = append(parts, "→ next q")
	}
	// Enter action depends on panel + position
	enter := "next"
	if state.currentIdx+1 == len(state.req.Clarifications) {
		enter = "go to review"
	}
	parts = append(parts, "Enter "+enter)
	parts = append(parts, "Esc abort")
	return "[" + strings.Join(parts, " | ") + "]"
}

// kindOrDefault returns kind, defaulting to "required" so a
// missing/empty Kind doesn't render as the empty string.
func kindOrDefault(k string) string {
	if k == "" {
		return protocol.ClarificationKindRequired
	}
	return k
}

// actionHint is the bottom action line — kept tight (single line)
// so the modal stays vertical-budget friendly.
func actionHint(s *inquiryState) string {
	switch s.req.Type {
	case protocol.InquiryTypeApproval:
		if s.replyMode {
			verb := s.replyVerb
			if verb == "" {
				verb = "approve"
			}
			return fmt.Sprintf("[type reason, Enter to /%s | esc to cancel]", verb)
		}
		return "[y] approve  [n] deny  [r] reply with reason  [esc] dismiss"
	case protocol.InquiryTypeClarification:
		return "[type answer, Enter to submit | esc to dismiss]"
	case protocol.InquiryTypeResearchBatch:
		return "[type one line per question, Enter to submit | esc to dismiss]"
	}
	return "[esc to dismiss]"
}

// wrap is a minimal word-wrapper for the modal body. Splits on
// whitespace, packs words into lines ≤ width. Existing newlines in
// the input are honoured (each paragraph wrapped independently).
func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	for i, para := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteString("\n")
		}
		var line strings.Builder
		for _, w := range strings.Fields(para) {
			if line.Len() == 0 {
				line.WriteString(w)
				continue
			}
			if line.Len()+1+len(w) > width {
				out.WriteString(line.String())
				out.WriteString("\n")
				line.Reset()
				line.WriteString(w)
				continue
			}
			line.WriteString(" ")
			line.WriteString(w)
		}
		if line.Len() > 0 {
			out.WriteString(line.String())
		}
	}
	return out.String()
}

var (
	inquiryBoxStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("11")).
			Padding(0, 1)
	inquiryTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("11"))
	inquiryFaintStyle = lipgloss.NewStyle().Faint(true)
	inquiryHintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
)
