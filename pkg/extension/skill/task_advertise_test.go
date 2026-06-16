package skill

import (
	"context"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// fakeTaskStore is a SkillStore that also implements the recall + log
// surfaces, so it can drive the full advertise split + `use` logging in
// one fixture. LogSkillEvents captures are mutex-guarded because the
// `shown` write fires from a goroutine.
type fakeTaskStore struct {
	skills  []skillpkg.Skill
	dynamic []skillpkg.RecallCandidate
	pinned  []skillpkg.RecallCandidate

	mu     sync.Mutex
	logged map[string][]string // event → skill ids
}

func (f *fakeTaskStore) List(context.Context) ([]skillpkg.Skill, error) { return f.skills, nil }

func (f *fakeTaskStore) Get(_ context.Context, name string) (skillpkg.Skill, error) {
	for _, s := range f.skills {
		if s.Manifest.Name == name {
			return s, nil
		}
	}
	return skillpkg.Skill{}, skillpkg.ErrSkillNotFound
}

func (f *fakeTaskStore) Publish(context.Context, skillpkg.Manifest, fs.FS, skillpkg.PublishOptions) error {
	return nil
}

func (f *fakeTaskStore) RecallRanked(_ context.Context, _ string, _ int) (dynamic, pinned []skillpkg.RecallCandidate, err error) {
	return f.dynamic, f.pinned, nil
}

func (f *fakeTaskStore) LogSkillEvents(_ context.Context, ids []string, event, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logged == nil {
		f.logged = map[string][]string{}
	}
	f.logged[event] = append(f.logged[event], ids...)
	return nil
}

func (f *fakeTaskStore) loggedFor(event string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logged[event]...)
}

func taskEligibleSkill(id, name, goal string) skillpkg.Skill {
	return skillpkg.Skill{
		ID: id,
		Manifest: skillpkg.Manifest{
			Name:        name,
			Description: name + " manifest description",
			Hugen: skillpkg.HugenMetadata{
				Task: skillpkg.TaskBlock{Eligible: true, Kind: "worker", GoalSummary: goal},
			},
		},
	}
}

func plainSkill(id, name string) skillpkg.Skill {
	return skillpkg.Skill{
		ID:       id,
		Manifest: skillpkg.Manifest{Name: name, Description: name + " desc"},
	}
}

// TestAdvertiseDraws_SplitsTaskAndSkill verifies the one-recall split: a
// pool of mixed task / non-task candidates yields a skills draw of the
// non-task names and a tasks draw of the task-eligible names — each ranked
// against its own population, neither crowding the other.
func TestAdvertiseDraws_SplitsTaskAndSkill(t *testing.T) {
	store := &fakeTaskStore{
		dynamic: []skillpkg.RecallCandidate{
			{ID: "s1", Name: "report_skill", Shown: 4, Used: 1},
			{ID: "s2", Name: "query_skill", Shown: 4, Used: 1},
			{ID: "t1", Name: "road_report", Shown: 4, Used: 2, TaskEligible: true},
			{ID: "t2", Name: "sales_digest", Shown: 4, Used: 2, TaskEligible: true},
		},
	}
	ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)
	seedRecap(t, ctx, state, "weekly road + sales reporting")

	draw, ok := ext.advertiseDraws(ctx, state, h)
	if !ok {
		t.Fatal("advertiseDraws should be ok with a recap anchor")
	}
	for _, name := range []string{"report_skill", "query_skill"} {
		if _, in := draw.skills[name]; !in {
			t.Errorf("skills draw missing non-task %q: %v", name, keys(draw.skills))
		}
		if _, in := draw.tasks[name]; in {
			t.Errorf("non-task %q leaked into tasks draw", name)
		}
	}
	for _, name := range []string{"road_report", "sales_digest"} {
		if _, in := draw.tasks[name]; !in {
			t.Errorf("tasks draw missing task %q: %v", name, keys(draw.tasks))
		}
		if _, in := draw.skills[name]; in {
			t.Errorf("task %q leaked into skills draw", name)
		}
	}
}

// TestTurnPreamble_RendersAvailableTasks verifies the `## Available tasks`
// block surfaces the advertised task with its goal_summary (not the
// manifest description) and that the skills block stays free of it.
func TestTurnPreamble_RendersAvailableTasks(t *testing.T) {
	store := &fakeTaskStore{
		skills: []skillpkg.Skill{
			plainSkill("s1", "report_skill"),
			taskEligibleSkill("t1", "road_report", "Summarise road segments in a region."),
		},
		dynamic: []skillpkg.RecallCandidate{
			{ID: "s1", Name: "report_skill", Shown: 2, Used: 1},
			{ID: "t1", Name: "road_report", Shown: 2, Used: 1, TaskEligible: true},
		},
	}
	ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	seedRecap(t, ctx, state, "road reporting")

	out := ext.TurnPreamble(ctx, state)
	skillsSection, tasksSection, split := strings.Cut(out, "## Available tasks")
	if !split {
		t.Fatalf("expected `## Available tasks` block:\n%s", out)
	}
	if strings.Contains(skillsSection, "road_report") {
		t.Errorf("task leaked into skills section:\n%s", skillsSection)
	}
	if !strings.Contains(tasksSection, "`road_report`") {
		t.Errorf("task missing from tasks section:\n%s", tasksSection)
	}
	if !strings.Contains(tasksSection, "Summarise road segments in a region.") {
		t.Errorf("task advertise should use goal_summary as the description:\n%s", tasksSection)
	}
}

// TestRecordTaskUsed_GatesOnShown verifies the bandit `use` gate: a task
// logs `used` only when it was advertised this session (resolvable via the
// task shown-catalogue), mirroring the skill-load path's used ≤ shown.
func TestRecordTaskUsed_GatesOnShown(t *testing.T) {
	store := &fakeTaskStore{
		skills: []skillpkg.Skill{
			taskEligibleSkill("t1", "road_report", "Roads."),
		},
	}
	ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)

	// Render the task catalogue to populate the task shown-catalogue.
	if _, ids := renderTaskCatalogue(ctx, state.Prompts(), h, nil); len(ids) != 1 {
		t.Fatalf("expected 1 advertised task id, got %v", ids)
	}

	// An advertised task logs exactly one `used`.
	h.RecordTaskUsed(ctx, "road_report")
	if got := store.loggedFor(skillpkg.SkillLogUsed); len(got) != 1 || got[0] != "t1" {
		t.Errorf("advertised task should log used=[t1], got %v", got)
	}

	// A task that was never advertised logs nothing (no shown-catalogue id).
	h.RecordTaskUsed(ctx, "never_shown")
	if got := store.loggedFor(skillpkg.SkillLogUsed); len(got) != 1 {
		t.Errorf("unadvertised task must not log a use; got %v", got)
	}
}
