package whiteboard

import (
	"strings"
	"testing"
	"time"
)

func TestProject_Empty(t *testing.T) {
	wb := Project(nil)
	if wb.Active {
		t.Errorf("Project on empty events should produce inactive board, got Active=true")
	}
	if len(wb.Messages) != 0 {
		t.Errorf("Project on empty events should have no messages, got %d", len(wb.Messages))
	}
}

func TestProject_InitWriteStop(t *testing.T) {
	t0 := time.Now().UTC()
	events := []ProjectEvent{
		{At: t0, Op: OpInit},
		{At: t0.Add(time.Second), Op: OpWrite, Seq: 1, FromSessionID: "ses-A", FromRole: "explorer", Text: "found auth_logs"},
		{At: t0.Add(2 * time.Second), Op: OpWrite, Seq: 2, FromSessionID: "ses-B", FromRole: "scout", Text: "checking schema"},
		{At: t0.Add(3 * time.Second), Op: OpStop},
	}
	wb := Project(events)
	if wb.Active {
		t.Errorf("Active=true after stop")
	}
	if !wb.StoppedAt.Equal(t0.Add(3 * time.Second)) {
		t.Errorf("StoppedAt = %v, want %v", wb.StoppedAt, t0.Add(3*time.Second))
	}
	if len(wb.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(wb.Messages))
	}
	if wb.Messages[0].Text != "found auth_logs" || wb.Messages[1].Text != "checking schema" {
		t.Errorf("messages out of order: %+v", wb.Messages)
	}
	if wb.NextSeq != 3 {
		t.Errorf("NextSeq = %d, want 3", wb.NextSeq)
	}
}

func TestProject_ReinitClearsMessages(t *testing.T) {
	t0 := time.Now().UTC()
	events := []ProjectEvent{
		{At: t0, Op: OpInit},
		{At: t0.Add(1), Op: OpWrite, Seq: 1, Text: "old"},
		{At: t0.Add(2), Op: OpStop},
		{At: t0.Add(3), Op: OpInit},
		{At: t0.Add(4), Op: OpWrite, Seq: 1, Text: "new"},
	}
	wb := Project(events)
	if !wb.Active {
		t.Errorf("Active should be true after second init")
	}
	if len(wb.Messages) != 1 || wb.Messages[0].Text != "new" {
		t.Errorf("Messages = %+v, want only 'new'", wb.Messages)
	}
}

func TestProject_WriteBeforeInitDropped(t *testing.T) {
	wb := Project([]ProjectEvent{
		{Op: OpWrite, Seq: 1, Text: "stray"},
	})
	if len(wb.Messages) != 0 {
		t.Errorf("write before init must not land in projection; got %+v", wb.Messages)
	}
}

func TestApply_TextTruncation(t *testing.T) {
	long := strings.Repeat("x", MaxMessageBytes+200)
	wb := Apply(Whiteboard{}, ProjectEvent{Op: OpInit})
	wb = Apply(wb, ProjectEvent{Op: OpWrite, Seq: 1, Text: long})
	if len(wb.Messages) != 1 {
		t.Fatalf("Messages len = %d", len(wb.Messages))
	}
	got := wb.Messages[0]
	if !got.Truncated {
		t.Errorf("Truncated should be true for %d-byte input", len(long))
	}
	if !strings.HasSuffix(got.Text, TruncationMarker) {
		t.Errorf("text did not end with truncation marker: tail=%q", got.Text[max(0, len(got.Text)-32):])
	}
	if len(got.Text) > MaxMessageBytes {
		t.Errorf("text retained %d bytes, want <= %d", len(got.Text), MaxMessageBytes)
	}
}

func TestApply_TruncatedFlagPreserved(t *testing.T) {
	// Caller already truncated at the wire layer; Truncated=true on
	// the event must survive even when the string itself fits.
	wb := Apply(Whiteboard{}, ProjectEvent{Op: OpInit})
	wb = Apply(wb, ProjectEvent{Op: OpWrite, Seq: 1, Text: "fits", Truncated: true})
	if !wb.Messages[0].Truncated {
		t.Errorf("Truncated flag dropped on apply")
	}
}

func TestEviction_FIFOByCount(t *testing.T) {
	wb := Apply(Whiteboard{}, ProjectEvent{Op: OpInit})
	for i := 0; i < MaxMessages+5; i++ {
		wb = Apply(wb, ProjectEvent{
			Op:   OpWrite,
			Seq:  int64(i + 1),
			Text: "m",
		})
	}
	if len(wb.Messages) != MaxMessages {
		t.Fatalf("retained %d messages, want %d", len(wb.Messages), MaxMessages)
	}
	// First retained message should be #6 (zero-based 5), since
	// 0..4 evicted to bring the ring down to 100.
	if wb.Messages[0].Seq != 6 {
		t.Errorf("oldest retained Seq = %d, want 6 after FIFO eviction", wb.Messages[0].Seq)
	}
	if wb.Messages[len(wb.Messages)-1].Seq != int64(MaxMessages+5) {
		t.Errorf("newest retained Seq = %d, want %d",
			wb.Messages[len(wb.Messages)-1].Seq, MaxMessages+5)
	}
}

func TestEviction_FIFOByBytes(t *testing.T) {
	wb := Apply(Whiteboard{}, ProjectEvent{Op: OpInit})
	// Each message is ~1 KiB; 40 messages would total 40 KiB > 32 KiB
	// total cap, so the oldest get evicted to fit.
	chunk := strings.Repeat("y", 1024)
	for i := 0; i < 40; i++ {
		wb = Apply(wb, ProjectEvent{Op: OpWrite, Seq: int64(i + 1), Text: chunk})
	}
	if wb.totalBytes > MaxTotalBytes {
		t.Errorf("totalBytes = %d, want <= %d", wb.totalBytes, MaxTotalBytes)
	}
	// At least one eviction should have happened.
	if len(wb.Messages) >= 40 {
		t.Errorf("byte cap did not evict: kept %d of 40 messages", len(wb.Messages))
	}
}

func TestApply_Chain_EquivalentToProject(t *testing.T) {
	t0 := time.Now().UTC()
	events := []ProjectEvent{
		{At: t0, Op: OpInit},
		{At: t0.Add(1), Op: OpWrite, Seq: 1, Text: "a"},
		{At: t0.Add(2), Op: OpWrite, Seq: 2, Text: "b"},
	}
	want := Project(events)
	got := Whiteboard{}
	for _, ev := range events {
		got = Apply(got, ev)
	}
	if got.Active != want.Active || len(got.Messages) != len(want.Messages) {
		t.Errorf("chain Apply diverged from Project: got=%+v want=%+v", got, want)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
