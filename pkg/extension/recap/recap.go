package recap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// OpSet is the ExtensionFrame op stamped on every folded recap. Recover
// replays the LAST set op to seed the compressed recap + watermark.
const OpSet = "set"

// recapResponseOverheadTokens is the headroom added to RecapTargetTokens
// for the rest of the JSON reply — the topic label, the keywords array,
// and the field names / braces / quotes — so the model isn't cut off
// mid-object after spending its budget on the recap text.
const recapResponseOverheadTokens = 128

// framePayload is the JSON shape persisted on a recap CategoryOp frame:
// the latest marker, replayed on restart.
type framePayload struct {
	Topic      string   `json:"topic,omitempty"`
	Text       string   `json:"text"`
	Categories []string `json:"categories,omitempty"`
}

// foldView is the typed payload assets/prompts/recap/topic.tmpl renders
// against: the prior marker (may be empty), the recent completed exchanges,
// and the turn's NEW user messages (possibly several).
type foldView struct {
	Prior  string
	Recent []foldTurnView
	New    []string
}

type foldTurnView struct {
	Role string
	Text string
}

// fold (re)forms the marker. Called synchronously (at most one at a time,
// guarded by beginRefresh) from [Extension.OnTurnBoundary] at every turn
// boundary. It gives the cheap model the prior marker ("what the dialogue
// is about"), the recent completed exchanges, and the turn's new user
// message(s), and stores the updated marker + emits a CategoryOp frame for
// restart-replay.
//
// Best-effort + timeout-bounded: a render / model / parse failure logs warn
// and leaves the previous marker in place (the raw recent ring still backs
// CurrentRecap, so the topic is never empty). The passed-in ctx (the turn's
// context) is bounded by BuildTimeout so a slow or hung summarizer can't
// stall the turn-start past that budget.
func (e *Extension) fold(ctx context.Context, state extension.SessionState, h *sessionRecap) {
	defer h.endRefresh()

	prior, recent, fresh := h.snapshotForFold(e.recentContext)
	if len(fresh) == 0 {
		return // no new user message this turn — nothing to (re)form
	}
	if state.Prompts() == nil || e.deps.Router == nil {
		return // boot-test fixture — nothing to fold
	}

	view := foldView{Prior: prior.Text, New: fresh}
	for _, m := range recent {
		view.Recent = append(view.Recent, foldTurnView{Role: m.Role, Text: m.Text})
	}
	body, err := state.Prompts().Render("recap/topic", view)
	if err != nil {
		e.deps.Logger.Warn("recap: render topic prompt failed", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, e.cfg.BuildTimeout)
	defer cancel()

	mdl, _, err := e.deps.Router.Resolve(ctx, model.Hint{Intent: e.cfg.Intent})
	if err != nil {
		e.deps.Logger.Warn("recap: resolve model failed", "err", err)
		return
	}
	raw, err := streamModelText(ctx, mdl, body, e.cfg.RecapTargetTokens+recapResponseOverheadTokens)
	if err != nil {
		e.deps.Logger.Warn("recap: model call failed", "err", err)
		return
	}
	topic, theme, categories, err := parseRecapResponse(raw)
	if err != nil {
		e.deps.Logger.Warn("recap: parse response failed", "err", err, "raw", truncate(raw, 200))
		return
	}

	rec := Recap{Topic: topic, Text: theme, Categories: categories}
	h.setMarker(rec)

	data, err := json.Marshal(framePayload{
		Topic:      rec.Topic,
		Text:       rec.Text,
		Categories: rec.Categories,
	})
	if err != nil {
		e.deps.Logger.Warn("recap: marshal frame failed", "err", err)
		return
	}
	frame := protocol.NewExtensionFrame(
		state.SessionID(),
		agentParticipant(e.deps.AgentID),
		providerName,
		protocol.CategoryOp,
		OpSet,
		data,
	)
	// Emit persists the frame (AppendEvent) so Recover can replay it.
	// Session.emit is goroutine-safe (the store serialises concurrent
	// emits via NextSeq + AppendEvent), so calling it from this
	// background goroutine is sound. A benign ErrSessionClosed during
	// teardown is logged at debug, not warn.
	if err := state.Emit(ctx, frame); err != nil {
		e.deps.Logger.Debug("recap: emit recap frame", "err", err)
	}
}

// parseRecapResponse extracts {topic, recap, keywords} from the model
// reply. Lenient: strips an optional ```json fence + surrounding prose,
// then unmarshals the first JSON object. Errors when no object parses or
// the (load-bearing) recap is empty; topic + keywords may be empty.
func parseRecapResponse(raw string) (topic, recap string, categories []string, err error) {
	s := stripFence(strings.TrimSpace(raw))
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return "", "", nil, fmt.Errorf("no JSON object in response")
	}
	var parsed struct {
		Topic    string   `json:"topic"`
		Recap    string   `json:"recap"`
		Keywords []string `json:"keywords"`
	}
	if uerr := json.Unmarshal([]byte(s[start:end+1]), &parsed); uerr != nil {
		return "", "", nil, fmt.Errorf("unmarshal: %w", uerr)
	}
	topic = strings.TrimSpace(parsed.Topic)
	recap = strings.TrimSpace(parsed.Recap)
	if recap == "" {
		return "", "", nil, fmt.Errorf("empty recap")
	}
	for _, k := range parsed.Keywords {
		if k = strings.TrimSpace(k); k != "" {
			categories = append(categories, k)
		}
	}
	return topic, recap, categories, nil
}

// streamModelText drives a single-turn pure-text model call: one
// user-role message in, accumulated content out. Mirrors the compactor's
// helper of the same name.
func streamModelText(ctx context.Context, mdl model.Model, body string, maxTokens int) (string, error) {
	stream, err := mdl.Generate(ctx, model.Request{
		Messages:  []model.Message{{Role: model.RoleUser, Content: body}},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("generate: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var buf strings.Builder
	for {
		chunk, more, err := stream.Next(ctx)
		if err != nil {
			return "", fmt.Errorf("stream: %w", err)
		}
		if chunk.Content != nil {
			buf.WriteString(*chunk.Content)
		}
		if !more {
			break
		}
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return "", fmt.Errorf("empty response")
	}
	return out, nil
}

func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func agentParticipant(agentID string) protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: agentID, Kind: protocol.ParticipantAgent, Name: "hugen"}
}
