package compactor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Pre-truncation caps applied to template inputs. The Renderer
// has no FuncMap (so no `trunc` template func); we cap in Go
// so the prompt body stays bounded even if a single
// tool_call / user_message has a runaway payload. Values track
// the spec's per-field limits in §5.3 (which themselves are
// budget-balanced for a ~2k summariser turn).
const (
	keptVerbatimMaxChars    = 800
	toolNameMaxChars        = 80
	toolArgsMaxChars        = 200
	toolResultMaxChars      = 400
	inquiryQuestionMaxChars = 200
	inquiryAnswerMaxChars   = 400
	priorBlockMaxChars      = 400
	summariseMaxTokens      = 1500
)

// toolCallPair is the bundled (call, result) shape the
// summariser template renders. Args / Result are pre-truncated
// strings so the template doesn't need a `trunc` func.
type toolCallPair struct {
	ToolName string
	Args     string
	Result   string
}

// inquirySegment is a labelled Q/A the summariser sees as
// "Inquiries handled in this window". Pre-truncated.
type inquirySegment struct {
	Question string
	Answer   string
}

// classified is the per-Kind dispatch result over the selected
// row range. Each field aligns with one column of the table in
// spec §5.2.
type classified struct {
	kept      []KeptSection
	toolPairs []toolCallPair
	inquiries []inquirySegment
	refs      []SubagentRef
}

// compact runs one full compaction iteration with the
// extension's top-level [Config], skipping the per-tier /
// per-skill resolver. Used by [Extension.OnTurnBoundary] for
// alpha-style fixtures and by the `/compactor compact` slash
// command — the latter wants the user-visible config the
// `/compactor status` line shows, not whatever the resolver
// would produce for the calling session.
//
// Callers that want the resolver applied call
// [Extension.compactWithConfig] directly.
func (e *Extension) compact(ctx context.Context, state extension.SessionState) error {
	return e.compactWithConfig(ctx, state, e.resolveTierConfig(ctx, state))
}

// compactWithConfig runs one full compaction iteration against
// the supplied resolved Config: range select, per-Kind dispatch,
// summariser LLM call, cap-driven collapse, persist, projection
// update. Errors bubble back to the boundary hook which logs and
// retries on the next boundary.
//
// γ: cfg is the resolver output computed once per fire. The body
// reads cfg.* exclusively — e.cfg.* references stay out of this
// path so the per-tier / per-skill / per-role overrides land
// uniformly.
func (e *Extension) compactWithConfig(ctx context.Context, state extension.SessionState, cfg Config) error {
	s := FromState(state)
	if s == nil {
		return nil
	}
	prior := s.Digest()

	// Compute the cutoff seq: the seq just before the k-th-
	// from-end user_message, where k = PreservedRecentTurns.
	// shouldCompact already gated on BoundaryCount.
	count := s.BoundaryCount()
	idx := count - cfg.PreservedRecentTurns - 1
	if idx < 0 {
		return nil
	}
	kthUserSeq := s.BoundaryAt(idx)
	newCutoff := kthUserSeq - 1
	priorCutoff := int64(0)
	if prior != nil {
		priorCutoff = prior.CutoffSeq
	}
	if newCutoff <= priorCutoff {
		// Nothing new to compact (preserved-window math hasn't
		// advanced past the prior cutoff yet).
		return nil
	}

	// Fetch rows in (priorCutoff, newCutoff].
	rows, err := e.deps.Store.ListEvents(ctx, state.SessionID(), store.ListEventsOpts{
		MinSeq: int(priorCutoff),
		Limit:  10_000,
	})
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}
	selected := make([]store.EventRow, 0, len(rows))
	for _, r := range rows {
		if int64(r.Seq) > newCutoff {
			break
		}
		selected = append(selected, r)
	}

	// Per-Kind dispatch (spec §5.2).
	c := classifyRows(selected)

	// Pure conversational ranges (no tool calls, no inquiries)
	// have nothing for the summariser to do — KeptVerbatim
	// already carries every high-signal turn. Skip the LLM
	// round-trip and stamp an informational placeholder so the
	// digest still surfaces the range boundary in Block C
	// without misleading the reader with a "summary failed"
	// marker.
	var (
		summary string
		llmErr  error
	)
	if len(c.toolPairs) == 0 && len(c.inquiries) == 0 {
		summary = fmt.Sprintf(
			"(no tool-call sequence or user inquiry in seq %d-%d; full chat turns preserved in Key turns above)",
			priorCutoff+1, newCutoff)
	} else {
		// Summariser LLM call. On error, fall back to a marker
		// block (spec §5.6) so high-signal content still survives
		// via KeptVerbatim.
		summary, llmErr = e.runSummariser(ctx, state, cfg, prior, c, priorCutoff+1, newCutoff)
		if llmErr != nil {
			e.logger.Warn("compactor: summariser failed; using fallback marker",
				"session", state.SessionID(), "err", llmErr)
			summary = fmt.Sprintf(
				"(LLM summary failed: %s; tool sequence in seq %d-%d was dropped from prompt — see full transcript for details)",
				llmErr.Error(), priorCutoff+1, newCutoff)
		}
	}

	// Build the next payload by appending to the prior.
	var (
		iteration     int
		summaryBlocks []SummaryBlock
		kept          []KeptSection
		refs          []SubagentRef
	)
	if prior != nil {
		iteration = prior.Iteration
		summaryBlocks = append(summaryBlocks, prior.SummaryBlocks...)
		kept = append(kept, prior.KeptVerbatim...)
		refs = append(refs, prior.SubagentRefs...)
	}
	iteration++
	// Pure-chat range (no tool calls, no inquiries) — KeptVerbatim
	// already carries every high-signal turn. Skipping the
	// SummaryBlock append avoids a bookkeeping-only "(no tool-call
	// sequence...)" entry in Block C that the model would otherwise
	// see. Subsequent compactions still find priorCutoff correctly
	// via CutoffSeq.
	pureChatRange := len(c.toolPairs) == 0 && len(c.inquiries) == 0
	if !pureChatRange {
		summaryBlocks = append(summaryBlocks, SummaryBlock{
			Iter: iteration,
			From: priorCutoff + 1,
			To:   newCutoff,
			Text: summary,
		})
	}
	kept = append(kept, c.kept...)
	refs = append(refs, c.refs...)

	// Cap KeptVerbatim at the configured ceiling. The first
	// user_message is pinned at index 0 so the model never loses
	// the original task framing; oldest non-pinned entries drop
	// FIFO. Spec §3.5 / §5.5.
	if cfg.KeptVerbatimMax > 0 {
		kept = pruneKept(kept, cfg.KeptVerbatimMax)
	}

	compactedAt := newCutoff
	if n := len(selected); n > 0 {
		if seq := int64(selected[n-1].Seq); seq > compactedAt {
			compactedAt = seq
		}
	}

	next := &DigestPayload{
		Version:         CurrentPayloadVersion,
		Iteration:       iteration,
		CutoffSeq:       newCutoff,
		CompactedAtSeq:  compactedAt,
		KeptVerbatim:    kept,
		SummaryBlocks:   summaryBlocks,
		SubagentRefs:    dedupRefs(refs),
		BuiltAt:         time.Now().UTC(),
		UIMarkerEnabled: cfg.UIMarkerEnabled,
	}

	// Cap check: collapse if the digest grew past
	// DigestMaxTokens (spec §5.5). Collapse failure leaves the
	// digest un-collapsed for this iteration; next fire retries.
	if cfg.DigestMaxTokens > 0 && estimateDigestTokens(next) > cfg.DigestMaxTokens {
		collapsed, err := e.runCollapse(ctx, state, cfg, next.SummaryBlocks, next.KeptVerbatim)
		if err != nil {
			e.logger.Warn("compactor: collapse failed; leaving digest over cap",
				"session", state.SessionID(), "err", err)
		} else {
			first := next.SummaryBlocks[0]
			next.SummaryBlocks = []SummaryBlock{{
				Iter: next.Iteration,
				From: first.From,
				To:   next.CutoffSeq,
				Text: collapsed,
			}}
		}
	}

	// Persist as an ExtensionFrame{op: digest_set} so Recovery
	// can replay it on restart.
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("marshal digest: %w", err)
	}
	frame := protocol.NewExtensionFrame(
		state.SessionID(),
		agentParticipant(e.deps.AgentID),
		providerName,
		protocol.CategoryOp,
		OpDigestSet,
		data,
	)
	if err := state.Emit(ctx, frame); err != nil {
		return fmt.Errorf("emit digest_set: %w", err)
	}
	s.SetDigest(next)
	// η.2 — strategy=summarize truncates the live history past
	// the new cutoff. The summarized range now lives only in
	// Block C (via Advertiser); the recent tail (Seq > cutoff)
	// stays verbatim. Other strategies don't reach this code
	// path (shouldCompact short-circuits on non-summarize), so
	// the unconditional call is safe.
	s.pruneToCutoff(next.CutoffSeq)
	return nil
}

// classifyRows bins each EventRow per spec §5.2. Tool call /
// result pairs are matched by tool_id from their metadata
// payload (round-tripped via the codec); unmatched calls /
// results are still surfaced with the available side so the
// summariser sees the partial info instead of dropping it.
func classifyRows(rows []store.EventRow) classified {
	var c classified

	// Index tool_call → row index, by tool_id, so a follow-up
	// tool_result can pair without scanning.
	type pendingCall struct {
		name string
		args string
	}
	pending := map[string]*pendingCall{}

	// Index inquiry_request → question, by request_id, so the
	// matching inquiry_response can pair (request_id rides
	// metadata via __request_id at row level too, but the
	// inquiry payload itself carries it explicitly).
	pendingInquiry := map[string]string{}

	for _, r := range rows {
		switch protocol.Kind(r.EventType) {
		case protocol.KindUserMessage:
			c.kept = append(c.kept, KeptSection{
				Kind:   "user_message",
				Author: r.Author,
				Seq:    int64(r.Seq),
				Text:   truncate(r.Content, keptVerbatimMaxChars),
			})
		case protocol.KindAgentMessage:
			// Only consolidated finals carry signal worth
			// keeping verbatim; streaming chunks never persist
			// anyway (kept guard is defence-in-depth).
			isFinal, _ := r.Metadata["final"].(bool)
			isConsolidated, _ := r.Metadata["consolidated"].(bool)
			if isFinal && isConsolidated && r.Content != "" {
				c.kept = append(c.kept, KeptSection{
					Kind:   "agent_message",
					Author: r.Author,
					Seq:    int64(r.Seq),
					Text:   truncate(r.Content, keptVerbatimMaxChars),
				})
			}
		case protocol.KindSystemMessage:
			if r.Content == "" {
				continue
			}
			c.kept = append(c.kept, KeptSection{
				Kind:   "system_message",
				Author: r.Author,
				Seq:    int64(r.Seq),
				Text:   truncate(r.Content, keptVerbatimMaxChars),
			})
		case protocol.KindError:
			// Terminal errors are user-visible context; keep.
			// Recoverable errors are noise; drop.
			rec, _ := r.Metadata["recoverable"].(bool)
			if rec {
				continue
			}
			c.kept = append(c.kept, KeptSection{
				Kind:   "error",
				Author: r.Author,
				Seq:    int64(r.Seq),
				Text:   truncate(r.Content, keptVerbatimMaxChars),
			})
		case protocol.KindSlashCommand:
			c.kept = append(c.kept, KeptSection{
				Kind:   "slash_command",
				Author: r.Author,
				Seq:    int64(r.Seq),
				Text:   truncate(r.Content, keptVerbatimMaxChars),
			})
		case protocol.KindToolCall:
			id, _ := r.Metadata["tool_id"].(string)
			argsStr := truncate(stringifyArgs(r.ToolArgs), toolArgsMaxChars)
			name := truncate(r.ToolName, toolNameMaxChars)
			if id == "" {
				// Unpaired tool_call; surface with empty
				// result so the summariser still sees it.
				c.toolPairs = append(c.toolPairs, toolCallPair{
					ToolName: name,
					Args:     argsStr,
					Result:   "",
				})
				continue
			}
			pending[id] = &pendingCall{name: name, args: argsStr}
		case protocol.KindToolResult:
			id, _ := r.Metadata["tool_id"].(string)
			resultStr := truncate(r.ToolResult, toolResultMaxChars)
			if id != "" {
				if call, ok := pending[id]; ok {
					c.toolPairs = append(c.toolPairs, toolCallPair{
						ToolName: call.name,
						Args:     call.args,
						Result:   resultStr,
					})
					delete(pending, id)
					continue
				}
			}
			// Unpaired tool_result; surface with empty name/args.
			c.toolPairs = append(c.toolPairs, toolCallPair{
				Result: resultStr,
			})
		case protocol.KindSubagentResult:
			reason, _ := r.Metadata["reason"].(string)
			sid, _ := r.Metadata["session_id"].(string)
			if sid == "" {
				// fall back to envelope's child id
				sid, _ = r.Metadata["__from_session"].(string)
			}
			c.refs = append(c.refs, SubagentRef{
				SessionID: sid,
				Reason:    truncate(reason, toolResultMaxChars),
			})
		case protocol.KindInquiryRequest:
			rid, _ := r.Metadata["request_id"].(string)
			q, _ := r.Metadata["question"].(string)
			if rid != "" {
				pendingInquiry[rid] = truncate(q, inquiryQuestionMaxChars)
			}
		case protocol.KindInquiryResponse:
			rid, _ := r.Metadata["request_id"].(string)
			ans := answerFromResponseMeta(r.Metadata)
			question := ""
			if rid != "" {
				if q, ok := pendingInquiry[rid]; ok {
					question = q
					delete(pendingInquiry, rid)
				}
			}
			truncQuestion := truncate(question, inquiryQuestionMaxChars)
			truncAnswer := truncate(ans, inquiryAnswerMaxChars)
			c.inquiries = append(c.inquiries, inquirySegment{
				Question: truncQuestion,
				Answer:   truncAnswer,
			})
			// Also surface as a KeptVerbatim entry — spec §11.2
			// requires the user's clarification answer survives
			// intact, not just as LLM-summarised input.
			c.kept = append(c.kept, KeptSection{
				Kind:   "inquiry_qa",
				Author: r.Author,
				Seq:    int64(r.Seq),
				Text:   formatInquiryQA(truncQuestion, truncAnswer),
			})
		default:
			// Drop reasoning / heartbeat / system_marker /
			// session_status / extension_frame / subagent_started.
			// Spec §5.2 — extensions own their own state via
			// Recovery; the model doesn't need the op-level events.
		}
	}

	// Flush any unmatched pending tool_calls with empty result.
	for _, call := range pending {
		c.toolPairs = append(c.toolPairs, toolCallPair{
			ToolName: call.name,
			Args:     call.args,
			Result:   "",
		})
	}

	return c
}

// stringifyArgs serialises a ToolArgs map to a compact JSON
// representation for the summariser prompt. Empty / nil → "".
func stringifyArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

// answerFromResponseMeta extracts the human-meaningful "answer"
// from an InquiryResponse payload metadata: the clarification
// text if present, otherwise an approved/denied label.
func answerFromResponseMeta(meta map[string]any) string {
	if resp, ok := meta["response"].(string); ok && resp != "" {
		return resp
	}
	if approved, ok := meta["approved"].(bool); ok {
		if approved {
			if reason, ok := meta["reason"].(string); ok && reason != "" {
				return "approved: " + reason
			}
			return "approved"
		}
		if reason, ok := meta["reason"].(string); ok && reason != "" {
			return "denied: " + reason
		}
		return "denied"
	}
	if timeout, ok := meta["timeout"].(bool); ok && timeout {
		return "(timed out)"
	}
	return ""
}

// truncate caps s to maxChars; the cap is approximate (UTF-8
// safe is overkill — the summariser tolerates a clipped tail
// just fine).
func truncate(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "…"
}

// truncateForPrior caps a prior SummaryBlock text for the
// re-prompt context — sized for the priorBlocks template
// section per spec §5.3.
func truncateForPrior(s string) string {
	return truncate(s, priorBlockMaxChars)
}

// estimateDigestTokens returns the running token estimate for
// the whole digest payload — sum of every SummaryBlock + every
// KeptVerbatim entry. Same char/4 heuristic the FrameObserver
// uses for the budget trigger limb; consistency keeps the
// math comparable across the two paths.
func estimateDigestTokens(d *DigestPayload) int {
	if d == nil {
		return 0
	}
	total := 0
	for _, b := range d.SummaryBlocks {
		total += (len(b.Text) + 3) / 4
	}
	for _, k := range d.KeptVerbatim {
		total += (len(k.Text) + 3) / 4
	}
	for _, r := range d.SubagentRefs {
		total += (len(r.Reason) + len(r.SessionID) + 7) / 4
	}
	return total
}

// formatInquiryQA renders an inquiry Q/A pair as a single
// verbatim entry for [KeptSection.Text]. Q on first line, A on
// second; empty question (unmatched response — defensive) drops
// the Q label so the answer still reads cleanly.
func formatInquiryQA(question, answer string) string {
	if question == "" {
		return "A: " + answer
	}
	return "Q: " + question + "\nA: " + answer
}

// pruneKept caps the KeptVerbatim slice at maxEntries while
// pinning the first user_message at index 0 — so the model
// never loses the original task framing even as a long-running
// session crosses many compaction iterations.
//
// Strategy: when the slice exceeds maxEntries, keep the first
// user_message (or, if none was recorded, the head entry) plus
// the most-recent `maxEntries-1` other entries. Order is
// preserved across the kept entries.
//
// Spec §3.5 / §5.5.
func pruneKept(kept []KeptSection, maxEntries int) []KeptSection {
	if maxEntries <= 0 || len(kept) <= maxEntries {
		return kept
	}
	// Find the first user_message — the pinned head. Falls back
	// to index 0 if for some reason no user_message is present
	// (defensive; the live path always opens a turn with one).
	pinIdx := 0
	for i, k := range kept {
		if k.Kind == "user_message" {
			pinIdx = i
			break
		}
	}
	tail := kept[len(kept)-(maxEntries-1):]
	out := make([]KeptSection, 0, maxEntries)
	out = append(out, kept[pinIdx])
	// Tail may overlap with the pin only when pinIdx falls inside
	// the tail window — in that case the dedup below drops the
	// duplicate so we never emit the same KeptSection twice.
	pinSeq := kept[pinIdx].Seq
	for _, k := range tail {
		if k.Seq == pinSeq {
			continue
		}
		out = append(out, k)
	}
	return out
}

// dedupRefs collapses duplicate SubagentRef entries by SessionID,
// keeping the latest reason (last-write-wins as we walked the
// log in order). The returned slice preserves first-seen order.
func dedupRefs(refs []SubagentRef) []SubagentRef {
	if len(refs) <= 1 {
		return refs
	}
	seen := make(map[string]int, len(refs))
	out := make([]SubagentRef, 0, len(refs))
	for _, r := range refs {
		if r.SessionID == "" {
			out = append(out, r)
			continue
		}
		if idx, ok := seen[r.SessionID]; ok {
			out[idx] = r // last-write-wins on reason
			continue
		}
		seen[r.SessionID] = len(out)
		out = append(out, r)
	}
	return out
}

// agentParticipant builds the ParticipantInfo the compactor
// stamps on emitted ExtensionFrames. Mirrors the mission ext
// pattern.
func agentParticipant(agentID string) protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: agentID, Kind: protocol.ParticipantAgent, Name: "hugen"}
}
