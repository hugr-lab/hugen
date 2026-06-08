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

func TestAppendTurn_TruncateAndWatermarkAndEvict(t *testing.T) {
	h := &sessionRecap{}
	// Per-message truncation to maxMsgChars.
	h.appendTurn(1, "user", "abcdefghij", 4, 1000) // → "abcd"
	if got := h.tail[0].Text; got != "abcd" {
		t.Errorf("message not truncated: got %q, want abcd", got)
	}
	// Already-folded seq (≤ watermark) is ignored.
	h.watermarkSeq = 5
	h.appendTurn(3, "user", "stale", 100, 1000)
	if len(h.tail) != 1 {
		t.Errorf("message at/under watermark must be dropped; tail=%d", len(h.tail))
	}
	// Window cap evicts oldest.
	h2 := &sessionRecap{}
	h2.appendTurn(1, "user", "oldest", 100, 12)
	h2.appendTurn(2, "user", "middle", 100, 12)
	h2.appendTurn(3, "user", "newest", 100, 12)
	for _, turn := range h2.tail {
		if turn.Text == "oldest" {
			t.Errorf("oldest should evict under window cap; tail=%+v", h2.tail)
		}
	}
}

func TestEffective_RecapPlusTail(t *testing.T) {
	h := &sessionRecap{}
	if _, ok := h.effective(); ok {
		t.Fatal("effective should be absent before any dialogue")
	}
	h.appendTurn(1, "user", "count roads by region", 4096, 16384)
	eff, ok := h.effective()
	if !ok || !strings.Contains(eff.Text, "count roads by region") {
		t.Fatalf("effective should carry the first message; got %+v", eff)
	}
	// Fold turns ≤ seq 1 into a compressed recap (long Text + short
	// Topic); seq 2 stays in the tail.
	h.appendTurn(2, "user", "only EMEA", 4096, 16384)
	h.commitFold(Recap{Topic: "roads by region", Text: "User wants road counts and lengths per region.", Categories: []string{"roads"}}, 1)
	if h.watermarkSeq != 1 {
		t.Errorf("watermark = %d, want 1", h.watermarkSeq)
	}
	eff, _ = h.effective()
	if eff.Topic != "roads by region" {
		t.Errorf("effective Topic = %q, want 'roads by region'", eff.Topic)
	}
	if !strings.Contains(eff.Text, "road counts and lengths") {
		t.Errorf("effective Text should carry the compressed long recap; got %q", eff.Text)
	}
	if !strings.Contains(eff.Text, "only EMEA") {
		t.Errorf("effective Text should carry the un-folded tail; got %q", eff.Text)
	}
	if strings.Contains(eff.Text, "count roads by region") {
		t.Errorf("folded message should be gone from the tail; got %q", eff.Text)
	}
	if !reflect.DeepEqual(eff.Categories, []string{"roads"}) {
		t.Errorf("categories = %v, want [roads]", eff.Categories)
	}
}

func TestNeedsFold(t *testing.T) {
	h := &sessionRecap{}
	h.appendTurn(1, "user", strings.Repeat("x", 50), 4096, 16384)
	if h.needsFold(100) {
		t.Error("50 chars should be under a 100-char threshold")
	}
	h.appendTurn(2, "user", strings.Repeat("y", 60), 4096, 16384)
	if !h.needsFold(100) {
		t.Error("110 chars should cross a 100-char threshold")
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

func TestRecover_ReplaysCompressedAndRebuildsTail(t *testing.T) {
	ext := NewExtension(Deps{}, Config{})
	state := fixture.NewTestSessionState("ses-root") // depth 0 → root
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	rows := []store.EventRow{
		recapFrameRow("label1", "topic1 long recap", []any{"a"}, 2), // older recap
		recapFrameRow("label2", "topic2 long recap", []any{"b"}, 4), // latest → wins, watermark 4
		msgRow(3, protocol.KindUserMessage, "folded — under watermark"),    // seq ≤ 4 → skipped on rebuild
		msgRow(5, protocol.KindUserMessage, "new question"),               // seq > 4 → tail
		msgRow(6, protocol.KindAgentMessage, "new answer"),                // seq > 4 → tail
	}
	if err := ext.Recover(context.Background(), state, rows); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	eff, ok := CurrentRecap(state)
	if !ok {
		t.Fatal("expected a recovered recap")
	}
	if eff.Topic != "label2" {
		t.Errorf("recovered Topic = %q, want 'label2' (latest wins)", eff.Topic)
	}
	if !strings.Contains(eff.Text, "topic2 long recap") {
		t.Errorf("compressed long recap = %q, want to contain 'topic2 long recap'", eff.Text)
	}
	if !strings.Contains(eff.Text, "new question") || !strings.Contains(eff.Text, "new answer") {
		t.Errorf("tail past watermark not rebuilt; got %q", eff.Text)
	}
	if strings.Contains(eff.Text, "folded") {
		t.Errorf("message under the watermark must NOT be in the rebuilt tail; got %q", eff.Text)
	}
	if !reflect.DeepEqual(eff.Categories, []string{"b"}) {
		t.Errorf("categories = %v, want [b]", eff.Categories)
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
		t.Error("no frames → no recap")
	}
}

func TestInitState_RootOnly(t *testing.T) {
	ext := NewExtension(Deps{}, Config{})
	ctx := context.Background()

	root := fixture.NewTestSessionState("ses-root") // depth 0
	_ = ext.InitState(ctx, root)
	if FromState(root) == nil {
		t.Error("root session must get a recap handle")
	}

	worker := fixture.NewTestSessionState("ses-w").WithDepth(1)
	_ = ext.InitState(ctx, worker)
	if FromState(worker) != nil {
		t.Error("non-root session must NOT get a recap handle")
	}
}

func TestOnFrameEmit_AccumulatesTail(t *testing.T) {
	ext := NewExtension(Deps{}, Config{}) // Router nil; small dialogue → no fold spawned
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	_ = ext.InitState(ctx, state)

	// Streaming chunk ignored; consolidated final recorded; user recorded.
	// Real frames carry seq ≥ 1 (the store allocates from 1); set them so
	// the watermark (0) doesn't drop them.
	stream := &protocol.AgentMessage{Payload: protocol.AgentMessagePayload{Text: "streaming", Consolidated: false}}
	stream.SetSeq(1)
	reply := &protocol.AgentMessage{Payload: protocol.AgentMessagePayload{Text: "prior reply", Consolidated: true, Final: true}}
	reply.SetSeq(2)
	user := &protocol.UserMessage{Payload: protocol.UserMessagePayload{Text: "do it"}}
	user.SetSeq(3)
	ext.OnFrameEmit(ctx, state, stream)
	ext.OnFrameEmit(ctx, state, reply)
	ext.OnFrameEmit(ctx, state, user)

	eff, ok := CurrentRecap(state)
	if !ok {
		t.Fatal("effective topic should exist after dialogue")
	}
	if !strings.Contains(eff.Text, "prior reply") || !strings.Contains(eff.Text, "do it") {
		t.Errorf("effective should carry assistant + user turns; got %q", eff.Text)
	}
	if strings.Contains(eff.Text, "streaming") {
		t.Errorf("streaming chunk must be ignored; got %q", eff.Text)
	}
}

func recapFrameRow(topic, text string, cats []any, watermark int) store.EventRow {
	return store.EventRow{
		EventType: string(protocol.KindExtensionFrame),
		Metadata: map[string]any{
			"extension": providerName,
			"op":        OpSet,
			"data":      map[string]any{"topic": topic, "text": text, "categories": cats, "watermark_seq": watermark},
		},
	}
}

func msgRow(seq int, kind protocol.Kind, content string) store.EventRow {
	return store.EventRow{Seq: seq, EventType: string(kind), Content: content}
}
