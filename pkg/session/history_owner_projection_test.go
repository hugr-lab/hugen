package session

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/extension/compactor"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// TestHistoryOwnerRecover_MatchesProjectHistory enforces phase
// 5.2.η.1's load-bearing equivalence: when the compactor
// recovers a session via [compactor.Extension.Recover], its
// owned history projection must reproduce the byte-for-byte
// slice that the legacy materialise path's [projectHistory]
// helper builds over the same events.
//
// Used by η.2 to flip the read path with confidence — until
// this stays green the compactor's ProvideHistory cannot
// legitimately replace the session-owned s.history slice.
//
// Fixtures exercise every shape projectHistory handles:
// user_message, agent_message{Consolidated} (with tool calls +
// thinking + signature), tool_result, system_message,
// subagent_started, subagent_result (default + async + silent).
func TestHistoryOwnerRecover_MatchesProjectHistory(t *testing.T) {
	rdr := testPrompts(t)
	cases := []struct {
		name string
		rows []EventRow
	}{
		{
			name: "user_then_agent_then_tools",
			rows: []EventRow{
				{Seq: 1, EventType: string(protocol.KindUserMessage), Content: "hi", CreatedAt: time.Unix(1, 0).UTC()},
				{
					Seq: 2, EventType: string(protocol.KindAgentMessage),
					Content: "ack with tool call",
					Metadata: map[string]any{
						"consolidated":      true,
						"thinking":          "let me check",
						"thought_signature": "sigA",
						"tool_calls": []any{
							map[string]any{
								"tool_id": "tc-1",
								"name":    "fs:read",
								"args":    map[string]any{"path": "/etc/hosts"},
							},
						},
					},
					CreatedAt: time.Unix(2, 0).UTC(),
				},
				{
					Seq: 3, EventType: string(protocol.KindToolResult),
					ToolResult: "/etc/hosts contents",
					Metadata: map[string]any{
						"tool_id": "tc-1",
					},
					CreatedAt: time.Unix(3, 0).UTC(),
				},
			},
		},
		{
			name: "system_message_and_subagents",
			rows: []EventRow{
				{
					Seq: 1, EventType: string(protocol.KindSystemMessage),
					Content:   "stuck nudge body",
					Metadata:  map[string]any{"kind": protocol.SystemMessageStuckNudge},
					CreatedAt: time.Unix(1, 0).UTC(),
				},
				{
					Seq: 2, EventType: string(protocol.KindSubagentStarted),
					Content: "explore the catalog",
					Metadata: map[string]any{
						"child_session_id": "sub-c1",
						"role":             "explorer",
						"depth":            float64(1),
					},
					CreatedAt: time.Unix(2, 0).UTC(),
				},
				{
					Seq: 3, EventType: string(protocol.KindSubagentResult),
					Content: "found 7 tables",
					Metadata: map[string]any{
						"session_id": "sub-c1",
						"reason":     protocol.TerminationCompleted,
						"turns_used": float64(4),
					},
					CreatedAt: time.Unix(3, 0).UTC(),
				},
			},
		},
		{
			name: "subagent_result_async_and_silent",
			rows: []EventRow{
				{
					Seq: 1, EventType: string(protocol.KindSubagentResult),
					Content: "async mission body",
					Metadata: map[string]any{
						"session_id":  "sub-async",
						"reason":      protocol.TerminationCompleted,
						"render_mode": protocol.SubagentRenderAsyncNotify,
						"goal":        "do the thing",
					},
					CreatedAt: time.Unix(1, 0).UTC(),
				},
				{
					Seq: 2, EventType: string(protocol.KindSubagentResult),
					Content: "silent",
					Metadata: map[string]any{
						"session_id":  "sub-silent",
						"reason":      protocol.TerminationCompleted,
						"render_mode": protocol.SubagentRenderSilent,
					},
					CreatedAt: time.Unix(2, 0).UTC(),
				},
				{Seq: 3, EventType: string(protocol.KindUserMessage), Content: "follow-up", CreatedAt: time.Unix(3, 0).UTC()},
			},
		},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := projectHistory(rdr, tc.rows, 10_000)

			ext := compactor.NewExtensionWithConfig(slog.Default(), compactor.DefaultConfig(), compactor.Deps{})
			state := newHistoryOwnerState(rdr)
			if err := ext.InitState(ctx, state); err != nil {
				t.Fatalf("InitState: %v", err)
			}
			if err := ext.Recover(ctx, state, tc.rows); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			owned := ext.ProvideHistory(ctx, state)

			if !reflect.DeepEqual(owned, legacy) {
				t.Fatalf("projection mismatch\nlegacy = %#v\nowner  = %#v", legacy, owned)
			}
		})
	}
}

// historyOwnerState is the minimal [extension.SessionState] the
// projection test needs: value-bag get/set + a Prompts hook so
// the projection's subagent_* templates render. Other surface
// methods short-circuit to zero values — Recover never touches
// them.
type historyOwnerState struct {
	id      string
	prompts *prompts.Renderer
	values  sync.Map
}

func newHistoryOwnerState(p *prompts.Renderer) *historyOwnerState {
	return &historyOwnerState{id: "test", prompts: p}
}

func (s *historyOwnerState) SessionID() string                      { return s.id }
func (s *historyOwnerState) SubagentName() string                   { return "" }
func (s *historyOwnerState) Role() string                           { return "" }
func (s *historyOwnerState) Skill() string                          { return "" }
func (s *historyOwnerState) Depth() int                             { return 0 }
func (s *historyOwnerState) Parent() (extension.SessionState, bool) { return nil, false }
func (s *historyOwnerState) Children() []extension.SessionState     { return nil }
func (s *historyOwnerState) Tools() *tool.ToolManager               { return nil }
func (s *historyOwnerState) Prompts() *prompts.Renderer             { return s.prompts }
func (s *historyOwnerState) Value(name string) (any, bool) {
	v, ok := s.values.Load(name)
	return v, ok
}
func (s *historyOwnerState) SetValue(name string, value any)                { s.values.Store(name, value) }
func (s *historyOwnerState) Emit(_ context.Context, _ protocol.Frame) error { return nil }
func (s *historyOwnerState) IsClosed() bool                                 { return false }
func (s *historyOwnerState) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} {
	return nil
}
func (s *historyOwnerState) OutboxOnly(_ context.Context, _ protocol.Frame) error { return nil }
func (s *historyOwnerState) Extensions() []extension.Extension                    { return nil }
func (s *historyOwnerState) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}
