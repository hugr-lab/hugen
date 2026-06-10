package recap

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

func TestAppendMessage_TruncateAndEvict(t *testing.T) {
	h := &sessionRecap{}
	// Per-message truncation to maxMsgChars.
	h.appendMessage("user", "abcdefghij", 4, 8) // → "abcd"
	if got := h.recent[0].Text; got != "abcd" {
		t.Errorf("message not truncated: got %q, want abcd", got)
	}
	// Ring eviction: keep only the last maxRing.
	h2 := &sessionRecap{}
	for _, m := range []string{"m1", "m2", "m3", "m4"} {
		h2.appendMessage("user", m, 100, 2)
	}
	if len(h2.recent) != 2 || h2.recent[0].Text != "m3" || h2.recent[1].Text != "m4" {
		t.Errorf("ring should keep the last 2; got %+v", h2.recent)
	}
}

func TestCurrent_MarkerOrRawFallback(t *testing.T) {
	h := &sessionRecap{}
	if _, ok := h.current(); ok {
		t.Fatal("current should be absent before any dialogue")
	}
	// No marker yet → raw ring fallback (so db-2 always has an anchor).
	h.appendMessage("user", "count roads by region", 4096, 8)
	cur, ok := h.current()
	if !ok || !strings.Contains(cur.Text, "count roads by region") {
		t.Fatalf("fallback should carry the raw ring; got %+v", cur)
	}
	if cur.Topic != "" {
		t.Errorf("raw fallback has no topic label; got %q", cur.Topic)
	}
	// Marker set → it supersedes the raw ring.
	h.setMarker(Recap{Topic: "roads by region", Text: "User wants road counts per region.", Categories: []string{"roads"}})
	cur, _ = h.current()
	if cur.Topic != "roads by region" || !strings.Contains(cur.Text, "road counts per region") {
		t.Errorf("marker should supersede the ring; got %+v", cur)
	}
}

func TestSnapshotForFold_SplitsRecentAndNew(t *testing.T) {
	h := &sessionRecap{}
	h.setMarker(Recap{Text: "prior theme"})
	// q1/a1 = a completed exchange; q2, q3 = trailing new user messages
	// (e.g. a spawn's goal+inputs, or a multi-message turn).
	h.appendMessage("user", "q1", 100, 8)
	h.appendMessage("assistant", "a1", 100, 8)
	h.appendMessage("user", "q2", 100, 8)
	h.appendMessage("user", "q3", 100, 8)

	prior, recent, fresh := h.snapshotForFold(4)
	if prior.Text != "prior theme" {
		t.Errorf("prior = %q, want 'prior theme'", prior.Text)
	}
	if !reflect.DeepEqual(fresh, []string{"q2", "q3"}) {
		t.Errorf("new = %v, want [q2 q3] (trailing user messages)", fresh)
	}
	if len(recent) != 2 || recent[0].Text != "q1" || recent[1].Text != "a1" {
		t.Errorf("recent context = %+v, want [q1 a1]", recent)
	}
}

func TestSessionRecap_InflightGuard(t *testing.T) {
	h := &sessionRecap{}
	if !h.beginRefresh() {
		t.Fatal("first beginRefresh should win")
	}
	if h.beginRefresh() {
		t.Fatal("second beginRefresh must be blocked while in-flight")
	}
	h.endRefresh()
	if !h.beginRefresh() {
		t.Fatal("beginRefresh should win again after endRefresh")
	}
}

func TestParseRecapResponse(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantTopic string
		wantRecap string
		wantCats  []string
		wantErr   bool
	}{
		{"plain", `{"topic":"roads by region","recap":"User wants road counts per region.","keywords":["tf","roads"]}`, "roads by region", "User wants road counts per region.", []string{"tf", "roads"}, false},
		{"fenced", "```json\n{\"topic\":\"x\",\"recap\":\"long x\",\"keywords\":[\"a\"]}\n```", "x", "long x", []string{"a"}, false},
		{"topic optional", `{"topic":"","recap":"some recap","keywords":[]}`, "", "some recap", nil, false},
		{"keyword trim+drop empty", `{"topic":"z","recap":"r","keywords":[" a ","",""," b "]}`, "z", "r", []string{"a", "b"}, false},
		{"empty recap err", `{"topic":"x","recap":"  ","keywords":["a"]}`, "", "", nil, true},
		{"no object err", `nothing here`, "", "", nil, true},
		{"bad json err", `{"recap": }`, "", "", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			topic, recap, cats, err := parseRecapResponse(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got topic=%q recap=%q", topic, recap)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if topic != c.wantTopic {
				t.Errorf("topic = %q, want %q", topic, c.wantTopic)
			}
			if recap != c.wantRecap {
				t.Errorf("recap = %q, want %q", recap, c.wantRecap)
			}
			if !reflect.DeepEqual(cats, c.wantCats) {
				t.Errorf("cats = %v, want %v", cats, c.wantCats)
			}
		})
	}
}

func TestRecover_ReplaysMarkerAndRebuildsRing(t *testing.T) {
	ext := NewExtension(Deps{}, Config{})
	state := fixture.NewTestSessionState("ses-root") // depth 0 → root
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	rows := []store.EventRow{
		recapFrameRow("label1", "marker one"), // older marker
		recapFrameRow("label2", "marker two"), // latest → wins
		msgRow(5, protocol.KindUserMessage, "recent question"),
		msgRow(6, protocol.KindAgentMessage, "recent answer"),
	}
	if err := ext.Recover(context.Background(), state, rows); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// CurrentRecap returns the latest replayed marker.
	cur, ok := CurrentRecap(state)
	if !ok {
		t.Fatal("expected a recovered marker")
	}
	if cur.Topic != "label2" || !strings.Contains(cur.Text, "marker two") {
		t.Errorf("recovered marker = %+v, want label2 / 'marker two' (latest wins)", cur)
	}
	// The ring was rebuilt from the message rows (fold context for the
	// next boundary).
	h := FromState(state)
	if len(h.recent) != 2 || h.recent[0].Text != "recent question" || h.recent[1].Text != "recent answer" {
		t.Errorf("ring not rebuilt from history; got %+v", h.recent)
	}
}

func TestRecover_NoFramesLeavesEmpty(t *testing.T) {
	ext := NewExtension(Deps{}, Config{})
	state := fixture.NewTestSessionState("ses-root")
	_ = ext.InitState(context.Background(), state)
	if err := ext.Recover(context.Background(), state, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, ok := CurrentRecap(state); ok {
		t.Error("no frames + no messages → no recap")
	}
}

func TestInitState_AllSessionsRootFlag(t *testing.T) {
	ext := NewExtension(Deps{}, Config{})
	ctx := context.Background()

	root := fixture.NewTestSessionState("ses-root") // depth 0
	_ = ext.InitState(ctx, root)
	if h := FromState(root); h == nil || !h.root {
		t.Error("root session must get a handle with root=true")
	}

	worker := fixture.NewTestSessionState("ses-w").WithDepth(1)
	_ = ext.InitState(ctx, worker)
	if h := FromState(worker); h == nil || h.root {
		t.Error("subagent must get a handle with root=false")
	}
}

func TestOnFrameEmit_AppendsRing(t *testing.T) {
	ext := NewExtension(Deps{}, Config{}) // Router nil → no fold; ring only
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	_ = ext.InitState(ctx, state)

	// Streaming chunk ignored; consolidated final recorded; user recorded.
	stream := &protocol.AgentMessage{Payload: protocol.AgentMessagePayload{Text: "streaming", Consolidated: false}}
	reply := &protocol.AgentMessage{Payload: protocol.AgentMessagePayload{Text: "prior reply", Consolidated: true, Final: true}}
	user := &protocol.UserMessage{Payload: protocol.UserMessagePayload{Text: "do it"}}
	// A system-synthetic user message (author=agent) — e.g. the async
	// summary kick — must be skipped.
	synth := protocol.NewUserMessage("ses-root", protocol.ParticipantInfo{ID: "ag", Kind: protocol.ParticipantAgent}, "summarize the mission")
	ext.OnFrameEmit(ctx, state, stream)
	ext.OnFrameEmit(ctx, state, reply)
	ext.OnFrameEmit(ctx, state, user)
	ext.OnFrameEmit(ctx, state, synth)

	cur, ok := CurrentRecap(state)
	if !ok {
		t.Fatal("topic should exist after dialogue")
	}
	if !strings.Contains(cur.Text, "prior reply") || !strings.Contains(cur.Text, "do it") {
		t.Errorf("ring should carry assistant + user turns; got %q", cur.Text)
	}
	if strings.Contains(cur.Text, "streaming") {
		t.Errorf("streaming chunk must be ignored; got %q", cur.Text)
	}
	if strings.Contains(cur.Text, "summarize the mission") {
		t.Errorf("agent-authored synthetic user message must be skipped; got %q", cur.Text)
	}
}

// TestOnFrameEmit_SubagentKeepsAgentAuthoredTask: the synthetic filter is
// ROOT-ONLY. A subagent's delegated task arrives as an agent-authored
// UserMessage (mission ext agentParticipant) and is the very text its
// marker distils — dropping it left every mission child markerless
// (dogfood 2026-06-10).
func TestOnFrameEmit_SubagentKeepsAgentAuthoredTask(t *testing.T) {
	ext := NewExtension(Deps{}, Config{})
	ctx := context.Background()
	worker := fixture.NewTestSessionState("ses-w").WithDepth(1)
	_ = ext.InitState(ctx, worker)

	task := protocol.NewUserMessage("ses-w",
		protocol.ParticipantInfo{ID: "hugen", Kind: protocol.ParticipantAgent},
		"run the tf roads-by-geozones query and build the table")
	ext.OnFrameEmit(ctx, worker, task)

	cur, ok := CurrentRecap(worker)
	if !ok || !strings.Contains(cur.Text, "roads-by-geozones") {
		t.Fatalf("subagent ring must keep its agent-authored task; got %+v ok=%v", cur, ok)
	}
}

func recapFrameRow(topic, text string) store.EventRow {
	return store.EventRow{
		EventType: string(protocol.KindExtensionFrame),
		Metadata: map[string]any{
			"extension": providerName,
			"op":        OpSet,
			"data":      map[string]any{"topic": topic, "text": text},
		},
	}
}

func msgRow(seq int, kind protocol.Kind, content string) store.EventRow {
	return store.EventRow{Seq: seq, EventType: string(kind), Content: content}
}
