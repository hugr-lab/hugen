package tui

import (
	"strings"
	"testing"
)

func TestChatBuffer_AppendUserAndRender(t *testing.T) {
	b := newChatBuffer()
	b.appendUser("alice", "hi there")
	out := b.render(80)
	if !strings.Contains(out, "alice") || !strings.Contains(out, "hi there") {
		t.Fatalf("render missing user content: %q", out)
	}
}

func TestChatBuffer_StreamingAssistantRendersInflight(t *testing.T) {
	b := newChatBuffer()
	b.appendAssistantChunk("partial reply")
	out := b.render(80)
	// glamour wraps each word in its own ANSI color span, so the
	// rendered output contains "partial" and "reply" separated by
	// escape sequences — test both tokens are present.
	if !strings.Contains(out, "partial") || !strings.Contains(out, "reply") {
		t.Fatalf("inflight assistant chunk missing: %q", out)
	}
	// Finalize → spans grow by one, pending reset.
	b.finalizeAssistant()
	if b.pendingAssistant.Len() != 0 {
		t.Fatalf("pendingAssistant should be empty after finalize")
	}
	if len(b.spans) != 1 || b.spans[0].kind != spanAssistant {
		t.Fatalf("finalize did not produce assistant span; spans=%d", len(b.spans))
	}
}

func TestChatBuffer_FinalizeEmpty_NoSpan(t *testing.T) {
	b := newChatBuffer()
	// Finalize with no pending content must not append a blank span.
	b.finalizeAssistant()
	b.finalizeReasoning()
	if len(b.spans) != 0 {
		t.Fatalf("empty finalize created spans: %d", len(b.spans))
	}
}

func TestChatBuffer_SystemSpanRendered(t *testing.T) {
	b := newChatBuffer()
	b.appendSystem("session terminated (user)")
	out := b.render(80)
	if !strings.Contains(out, "session terminated") {
		t.Fatalf("system span missing: %q", out)
	}
}

func TestPrefixMultiline_KeepsNewlinesAndIndents(t *testing.T) {
	got := prefixMultiline("thinking: ", "line one\nline two\r\nline three")
	want := "thinking: line one\n          line two\n          line three"
	if got != want {
		t.Fatalf("prefixMultiline =\n%q\nwant\n%q", got, want)
	}
}

func TestParseSlashFrame_DispatchPath(t *testing.T) {
	// Sanity: console.IsSlashCommand + ParseSlashCommand contract
	// powers model.dispatchUserInput. Direct call ensures the
	// import + signatures remain wired.
	m, submitted := newTestModel(t)
	if err := m.currentTab().dispatchUserInput("/end now"); err != nil {
		t.Fatalf("dispatchUserInput slash returned err: %v", err)
	}
	got := submitted.Load()
	if got == nil {
		t.Fatalf("nothing submitted")
	}
}
