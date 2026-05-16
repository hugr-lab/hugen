package session

import (
	"context"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
)

// fakeInstructor returns a fixed block, optionally empty.
type fakeInstructor struct {
	name string
	body string
}

func (f *fakeInstructor) Name() string { return f.name }
func (f *fakeInstructor) PerTurnPrompt(_ context.Context, _ extension.SessionState) string {
	return f.body
}

// TestPerTurnInstructorBlock_Joins concatenates two non-empty
// blocks in registration order and skips empty ones.
func TestPerTurnInstructorBlock_Joins(t *testing.T) {
	s := &Session{
		deps: &Deps{
			Extensions: []extension.Extension{
				&fakeInstructor{name: "a", body: "block-A"},
				&fakeInstructor{name: "empty"},
				&fakeInstructor{name: "b", body: "block-B"},
			},
		},
	}
	got := s.perTurnInstructorBlock(context.Background())
	want := "block-A\n\nblock-B"
	if got != want {
		t.Errorf("perTurnInstructorBlock = %q; want %q", got, want)
	}
}

// TestPerTurnInstructorBlock_EmptyWhenAllSkip — every Instructor
// returning "" produces no inject.
func TestPerTurnInstructorBlock_EmptyWhenAllSkip(t *testing.T) {
	s := &Session{
		deps: &Deps{
			Extensions: []extension.Extension{
				&fakeInstructor{name: "a"},
				&fakeInstructor{name: "b"},
			},
		},
	}
	if got := s.perTurnInstructorBlock(context.Background()); got != "" {
		t.Errorf("expected empty inject; got %q", got)
	}
}

// TestBuildMessages_InjectsBeforeLastUser pins the phase 5.2 π
// placement: the per-turn inject lands as a separate system-role
// message IMMEDIATELY BEFORE the trailing user message so the
// model sees the live state in its high-attention zone while the
// cache prefix (statics + history sans last user) stays stable.
// Every Session has a static system prompt at index 0 (at minimum
// the "Session tier: <tier>" header even without an agent), so
// the expected layout for a 3-turn history with the inject is:
//
//	[system: static]
//	[user: turn-1]
//	[assistant: ans-1]
//	[system: inject]
//	[user: turn-2 current]
func TestBuildMessages_InjectsBeforeLastUser(t *testing.T) {
	s := &Session{
		deps: &Deps{
			Extensions: []extension.Extension{
				&fakeInstructor{name: "liveview", body: "live"},
			},
		},
		history: []model.Message{
			{Role: model.RoleUser, Content: "turn-1"},
			{Role: model.RoleAssistant, Content: "ans-1"},
			{Role: model.RoleUser, Content: "turn-2 (current)"},
		},
	}
	out := s.buildMessages(context.Background())
	if len(out) != 5 {
		t.Fatalf("len(out) = %d; want 5 (static + history-3 + inject-1)", len(out))
	}
	if out[0].Role != model.RoleSystem {
		t.Errorf("out[0] expected static system prompt; got %+v", out[0])
	}
	if out[1].Role != model.RoleUser || out[1].Content != "turn-1" {
		t.Errorf("out[1] = %+v; want user turn-1", out[1])
	}
	if out[2].Role != model.RoleAssistant {
		t.Errorf("out[2].Role = %q; want assistant", out[2].Role)
	}
	if out[3].Role != model.RoleSystem || !strings.Contains(out[3].Content, "live") {
		t.Errorf("out[3] expected to be the inject; got %+v", out[3])
	}
	if out[4].Role != model.RoleUser || out[4].Content != "turn-2 (current)" {
		t.Errorf("out[4] = %+v; want last user message", out[4])
	}
}

// TestBuildMessages_InjectAppendedWhenLastNotUser — if the trailing
// message is assistant or tool (e.g. mid-loop rebuild between tool
// dispatch and the next model iteration), the inject appends at
// the end instead of being squeezed before a non-user slot.
func TestBuildMessages_InjectAppendedWhenLastNotUser(t *testing.T) {
	s := &Session{
		deps: &Deps{
			Extensions: []extension.Extension{
				&fakeInstructor{name: "liveview", body: "live"},
			},
		},
		history: []model.Message{
			{Role: model.RoleUser, Content: "turn-1"},
			{Role: model.RoleAssistant, Content: "ans-1"},
		},
	}
	out := s.buildMessages(context.Background())
	if len(out) != 4 {
		t.Fatalf("len(out) = %d; want 4 (static + history-2 + inject-1)", len(out))
	}
	if out[3].Role != model.RoleSystem || out[3].Content != "live" {
		t.Errorf("expected inject as trailing system message; got %+v", out[3])
	}
}

// TestBuildMessages_NoInjectWhenInstructorsSilent — if every
// Instructor returns "", buildMessages emits only the static
// system prompt + history verbatim.
func TestBuildMessages_NoInjectWhenInstructorsSilent(t *testing.T) {
	s := &Session{
		deps: &Deps{
			Extensions: []extension.Extension{
				&fakeInstructor{name: "silent"},
			},
		},
		history: []model.Message{
			{Role: model.RoleUser, Content: "hello"},
		},
	}
	out := s.buildMessages(context.Background())
	if len(out) != 2 {
		t.Fatalf("len(out) = %d; want 2 (static + history-1)", len(out))
	}
	if out[0].Role != model.RoleSystem {
		t.Errorf("out[0] expected static system; got %+v", out[0])
	}
	if out[1].Role != model.RoleUser {
		t.Errorf("out[1] expected user; got %+v", out[1])
	}
}
