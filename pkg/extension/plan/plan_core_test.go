package plan

import (
	"strings"
	"testing"
	"time"
)

func mkAt(offsetSeconds int) time.Time {
	base := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(offsetSeconds) * time.Second)
}

// TestProject_HappyPath exercises the canonical replay flow: a set
// event followed by three comments. The latest set is the boundary;
// every comment after it accumulates with caps applied.
func TestProject_HappyPath(t *testing.T) {
	events := []ProjectEvent{
		{At: mkAt(0), Op: OpSet, Text: "investigate latency", CurrentStep: "scope"},
		{At: mkAt(1), Op: OpComment, Text: "checked cache headers", CurrentStep: "scope"},
		{At: mkAt(2), Op: OpComment, Text: "instrumented handler", CurrentStep: "instrument"},
		{At: mkAt(3), Op: OpComment, Text: "found 80% in db wait"},
	}
	p := Project(events)
	if !p.Active {
		t.Fatal("expected active plan")
	}
	if p.Text != "investigate latency" {
		t.Errorf("Text = %q, want %q", p.Text, "investigate latency")
	}
	if p.CurrentStep != "instrument" {
		t.Errorf("CurrentStep = %q, want %q (last non-empty pointer)", p.CurrentStep, "instrument")
	}
	if len(p.Comments) != 3 {
		t.Errorf("len(Comments) = %d, want 3", len(p.Comments))
	}
	if !p.SetAt.Equal(mkAt(0)) {
		t.Errorf("SetAt = %v, want %v", p.SetAt, mkAt(0))
	}
	if !p.UpdatedAt.Equal(mkAt(3)) {
		t.Errorf("UpdatedAt = %v, want %v", p.UpdatedAt, mkAt(3))
	}
}

// TestProject_ReplaceBodyResetsComments verifies that the second
// "set" wipes the comment log: Project's algorithm uses the LATEST
// set as boundary and only accumulates comments after it.
func TestProject_ReplaceBodyResetsComments(t *testing.T) {
	events := []ProjectEvent{
		{At: mkAt(0), Op: OpSet, Text: "v1"},
		{At: mkAt(1), Op: OpComment, Text: "a"},
		{At: mkAt(2), Op: OpComment, Text: "b"},
		{At: mkAt(3), Op: OpSet, Text: "v2", CurrentStep: "redo"},
		{At: mkAt(4), Op: OpComment, Text: "c"},
	}
	p := Project(events)
	if p.Text != "v2" {
		t.Errorf("Text = %q, want %q", p.Text, "v2")
	}
	if p.CurrentStep != "redo" {
		t.Errorf("CurrentStep = %q, want %q", p.CurrentStep, "redo")
	}
	if len(p.Comments) != 1 || p.Comments[0].Text != "c" {
		t.Errorf("Comments = %+v, want [c]", p.Comments)
	}
}

// TestProject_ClearBoundary verifies that a "clear" op terminates
// the projection: any preceding set / comments are dropped because
// the latest boundary is "clear".
func TestProject_ClearBoundary(t *testing.T) {
	events := []ProjectEvent{
		{At: mkAt(0), Op: OpSet, Text: "v1"},
		{At: mkAt(1), Op: OpComment, Text: "a"},
		{At: mkAt(2), Op: OpClear},
	}
	p := Project(events)
	if p.Active {
		t.Errorf("expected inactive plan after clear; got %+v", p)
	}
	if p.Text != "" || len(p.Comments) != 0 {
		t.Errorf("expected zero plan; got Text=%q Comments=%v", p.Text, p.Comments)
	}
}

// TestProject_NoEvents returns an inactive plan.
func TestProject_NoEvents(t *testing.T) {
	if p := Project(nil); p.Active {
		t.Errorf("expected inactive plan from nil events; got %+v", p)
	}
}

// TestProject_BodyTruncation overflows MaxBodySize and asserts the
// truncation marker is appended.
func TestProject_BodyTruncation(t *testing.T) {
	big := strings.Repeat("x", MaxBodySize+200)
	events := []ProjectEvent{
		{At: mkAt(0), Op: OpSet, Text: big},
	}
	p := Project(events)
	if len(p.Text) > MaxBodySize {
		t.Errorf("len(Text) = %d, want ≤ %d", len(p.Text), MaxBodySize)
	}
	if !strings.HasSuffix(p.Text, TruncationMarker) {
		t.Errorf("expected truncation marker suffix, got tail %q",
			p.Text[max(0, len(p.Text)-len(TruncationMarker)*2):])
	}
}

// TestProject_CommentTruncation overflows MaxCommentSize.
func TestProject_CommentTruncation(t *testing.T) {
	big := strings.Repeat("y", MaxCommentSize+200)
	events := []ProjectEvent{
		{At: mkAt(0), Op: OpSet, Text: "ok"},
		{At: mkAt(1), Op: OpComment, Text: big},
	}
	p := Project(events)
	if len(p.Comments) != 1 {
		t.Fatalf("len(Comments) = %d, want 1", len(p.Comments))
	}
	c := p.Comments[0]
	if len(c.Text) > MaxCommentSize {
		t.Errorf("len(comment.Text) = %d, want ≤ %d", len(c.Text), MaxCommentSize)
	}
	if !strings.HasSuffix(c.Text, TruncationMarker) {
		t.Errorf("expected truncation marker on comment")
	}
}

// TestProject_FIFOEviction adds 35 comments and asserts the 30
// most-recent ones survive the projection cap.
func TestProject_FIFOEviction(t *testing.T) {
	events := []ProjectEvent{{At: mkAt(0), Op: OpSet, Text: "p"}}
	for i := 0; i < 35; i++ {
		events = append(events, ProjectEvent{
			At:   mkAt(i + 1),
			Op:   OpComment,
			Text: stringFromInt(i),
		})
	}
	p := Project(events)
	if len(p.Comments) != MaxComments {
		t.Fatalf("len(Comments) = %d, want %d", len(p.Comments), MaxComments)
	}
	// Oldest survivor should be comment #5 (35 - 30 = 5).
	if p.Comments[0].Text != stringFromInt(5) {
		t.Errorf("Comments[0].Text = %q, want %q (oldest after FIFO)",
			p.Comments[0].Text, stringFromInt(5))
	}
	if p.Comments[len(p.Comments)-1].Text != stringFromInt(34) {
		t.Errorf("Comments[-1].Text = %q, want %q",
			p.Comments[len(p.Comments)-1].Text, stringFromInt(34))
	}
}

// TestApply_MatchesProject verifies Apply is incrementally
// equivalent to Project on the prefix-then-event for a few shapes.
func TestApply_MatchesProject(t *testing.T) {
	events := []ProjectEvent{
		{At: mkAt(0), Op: OpSet, Text: "v1", CurrentStep: "a"},
		{At: mkAt(1), Op: OpComment, Text: "c1"},
		{At: mkAt(2), Op: OpComment, Text: "c2", CurrentStep: "b"},
	}
	want := Project(events)

	got := Plan{}
	for _, ev := range events {
		got = Apply(got, ev)
	}
	if !plansEqual(got, want) {
		t.Errorf("Apply chain ≠ Project; got=%+v want=%+v", got, want)
	}
}

// TestApply_CommentOnInactiveIsNoop guards against the tool layer
// missing the no_active_plan check: Apply itself returns the input
// plan unchanged rather than fabricating an Active=false Plan with
// comments.
func TestApply_CommentOnInactiveIsNoop(t *testing.T) {
	got := Apply(Plan{}, ProjectEvent{Op: OpComment, Text: "x"})
	if got.Active {
		t.Errorf("expected inactive after comment on inactive; got %+v", got)
	}
	if len(got.Comments) != 0 {
		t.Errorf("expected no comments; got %+v", got.Comments)
	}
}

// TestRender_ActivePlan covers the layout invariants.
func TestRender_ActivePlan(t *testing.T) {
	p := Plan{
		Active:      true,
		Text:        "investigate cache",
		CurrentStep: "instrument",
	}
	out := Render(planTestRenderer(t), p)
	if !strings.HasPrefix(out, "## Active plan\n") {
		t.Errorf("Render = %q, expected to start with ## Active plan", out)
	}
	if !strings.Contains(out, "Current focus: instrument") {
		t.Errorf("Render = %q, expected current focus line", out)
	}
	if !strings.Contains(out, "\ninvestigate cache") {
		t.Errorf("Render = %q, expected body", out)
	}
}

// TestRender_NoCurrentStepOmitsLine drops the "Current focus" line
// when no pointer is set.
func TestRender_NoCurrentStepOmitsLine(t *testing.T) {
	p := Plan{Active: true, Text: "body"}
	out := Render(planTestRenderer(t), p)
	if strings.Contains(out, "Current focus") {
		t.Errorf("Render = %q, expected no current focus line", out)
	}
}

// TestRender_InactiveReturnsEmpty so callers can drop the block on
// a nil-empty check.
func TestRender_InactiveReturnsEmpty(t *testing.T) {
	if out := Render(planTestRenderer(t), Plan{}); out != "" {
		t.Errorf("Render(inactive) = %q, want empty", out)
	}
}

// ---------- helpers ----------

func stringFromInt(i int) string {
	return "c-" + intToString(i)
}

func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

func plansEqual(a, b Plan) bool {
	if a.Active != b.Active || a.Text != b.Text || a.CurrentStep != b.CurrentStep {
		return false
	}
	if !a.SetAt.Equal(b.SetAt) || !a.UpdatedAt.Equal(b.UpdatedAt) {
		return false
	}
	if len(a.Comments) != len(b.Comments) {
		return false
	}
	for i := range a.Comments {
		x, y := a.Comments[i], b.Comments[i]
		if x.Text != y.Text || x.CurrentStep != y.CurrentStep || !x.At.Equal(y.At) {
			return false
		}
	}
	return true
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
