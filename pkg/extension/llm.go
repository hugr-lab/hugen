package extension

import (
	"context"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// StreamModelText drives a single-turn pure-text model call: one
// user-role message in, the accumulated content out. The shared shape
// every extension-side summariser uses (compactor digests, recap fold,
// hide briefs) — bound the call with a context deadline at the caller.
func StreamModelText(ctx context.Context, mdl model.Model, body string, maxTokens int) (string, error) {
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
		return "", fmt.Errorf("empty model response")
	}
	return out, nil
}

// TruncateChars caps s to maxChars bytes, marking the cut with an
// ellipsis. maxChars <= 0 disables the cap. The cap is approximate
// (byte-, not rune-accurate) — it sizes log lines and summariser
// inputs, which tolerate a clipped tail.
func TruncateChars(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "…"
}

// AgentParticipant builds the ParticipantInfo runtime extensions stamp
// on the frames they synthesise (extension frames, injected prompts).
func AgentParticipant(agentID string) protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: agentID, Kind: protocol.ParticipantAgent, Name: "hugen"}
}
