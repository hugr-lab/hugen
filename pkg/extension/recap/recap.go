package recap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/store/queries"
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
// the compressed recap fields plus the watermark seq so a restart can
// rebuild the tail from history past it.
type framePayload struct {
	Topic            string   `json:"topic,omitempty"`
	Text             string   `json:"text"`
	Categories       []string `json:"categories,omitempty"`
	ChangeConfidence float64  `json:"change_confidence,omitempty"`
	WatermarkSeq     int64    `json:"watermark_seq"`
}

// foldView is the typed payload assets/prompts/recap/topic.tmpl renders
// against: the prior compressed topic (may be empty) + the un-folded
// tail messages.
type foldView struct {
	Prior string
	Turns []foldTurnView
}

type foldTurnView struct {
	Role string
	Text string
}

// fold is the async summarizer body. Spawned (at most one at a time)
// from the FrameObserver when the tail crosses the fold threshold; runs
// on its own goroutine so the conversation never blocks. It snapshots
// the prior compressed topic + tail, asks the cheap model for a 3-5 word
// updated topic, derives change_confidence from the previous compressed
// topic, commits the fold (advancing the watermark, dropping folded tail
// turns) and emits a CategoryOp frame for restart-replay.
//
// Best-effort + timeout-bounded: a render / model / parse failure logs
// warn and leaves the previous recap + the (still-growing) tail in place
// — the effective topic stays usable from recap ⊕ tail regardless. ctx
// is a fresh background context bounded by BuildTimeout (the observer's
// ctx may already be cancelled).
func (e *Extension) fold(state extension.SessionState, h *sessionRecap) {
	defer h.endRefresh()

	prevText, turns, hiSeq := h.snapshotForFold()
	if len(turns) == 0 {
		return
	}
	if state.Prompts() == nil || e.deps.Router == nil {
		return // boot-test fixture — nothing to fold
	}

	view := foldView{Prior: prevText, Turns: make([]foldTurnView, 0, len(turns))}
	for _, t := range turns {
		view.Turns = append(view.Turns, foldTurnView{Role: t.Role, Text: t.Text})
	}
	body, err := state.Prompts().Render("recap/topic", view)
	if err != nil {
		e.deps.Logger.Warn("recap: render topic prompt failed", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.BuildTimeout)
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
	topic, recapLong, categories, err := parseRecapResponse(raw)
	if err != nil {
		e.deps.Logger.Warn("recap: parse response failed", "err", err, "raw", truncate(raw, 200))
		return
	}

	rec := Recap{Topic: topic, Text: recapLong, Categories: categories}
	// change_confidence = embedding distance(prev LONG recap, new LONG
	// recap) — the substantive content drift, not the coarse topic label.
	// Best-effort: no embedder / query failure leaves it 0.
	if prevText != "" && e.deps.Querier != nil {
		if d, derr := embeddingDistance(ctx, e.deps.Querier, e.cfg.EmbedModel, prevText, recapLong); derr == nil {
			rec.ChangeConfidence = d
		} else {
			e.deps.Logger.Debug("recap: embedding_distance unavailable", "err", derr)
		}
	}

	// Commit the fold (advance watermark, drop folded tail) and persist.
	h.commitFold(rec, hiSeq)

	data, err := json.Marshal(framePayload{
		Topic:            rec.Topic,
		Text:             rec.Text,
		Categories:       rec.Categories,
		ChangeConfidence: rec.ChangeConfidence,
		WatermarkSeq:     hiSeq,
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

// embeddingDistance calls the server-side Hugr function that embeds both
// phrases with `model` and returns their cosine distance — same embedder
// as vector search, no client-side vectors.
func embeddingDistance(ctx context.Context, q types.Querier, embedModel, a, b string) (float64, error) {
	dist, err := queries.RunQuery[float64](ctx, q,
		`query ($model: String!, $t1: String!, $t2: String!) {
			function { core { models {
				embedding_distance(model: $model, text1: $t1, text2: $t2, distance: Cosine)
			}}}
		}`,
		map[string]any{"model": embedModel, "t1": a, "t2": b},
		"function.core.models.embedding_distance",
	)
	if err != nil {
		return 0, err
	}
	return dist, nil
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
