package model

import (
	"context"
	"errors"
	"testing"
)

// fakeModel records its spec; Generate is unused in router tests.
type fakeModel struct{ spec ModelSpec }

func (f *fakeModel) Spec() ModelSpec                                       { return f.spec }
func (f *fakeModel) Generate(_ context.Context, _ Request) (Stream, error) { return nil, nil }

func newRouter(t *testing.T, defaults map[Intent]ModelSpec) *ModelRouter {
	t.Helper()
	models := make(map[ModelSpec]Model)
	for _, spec := range defaults {
		models[spec] = &fakeModel{spec: spec}
	}
	r, err := NewModelRouter(defaults, models)
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}
	return r
}

func TestRouter_FiveStepResolution(t *testing.T) {
	specA := ModelSpec{Provider: "p", Name: "a"}
	specB := ModelSpec{Provider: "p", Name: "b"}
	specC := ModelSpec{Provider: "p", Name: "c"}
	specD := ModelSpec{Provider: "p", Name: "d"}
	specE := ModelSpec{Provider: "p", Name: "e"}

	defaults := map[Intent]ModelSpec{
		IntentDefault: specE,
		IntentCheap:   specD,
	}

	cases := []struct {
		name string
		hint Hint
		want ModelSpec
	}{
		{
			name: "step 1 override wins",
			hint: Hint{
				Intent:        IntentDefault,
				ModelOverride: &specA,
				SessionModels: map[Intent]ModelSpec{IntentDefault: specB},
				SkillModels:   map[Intent]ModelSpec{IntentDefault: specC},
			},
			want: specA,
		},
		{
			name: "step 2 session overrides",
			hint: Hint{
				Intent:        IntentDefault,
				SessionModels: map[Intent]ModelSpec{IntentDefault: specB},
				SkillModels:   map[Intent]ModelSpec{IntentDefault: specC},
			},
			want: specB,
		},
		{
			name: "step 3 skill overrides",
			hint: Hint{
				Intent:      IntentDefault,
				SkillModels: map[Intent]ModelSpec{IntentDefault: specC},
			},
			want: specC,
		},
		{
			name: "step 4 runtime default",
			hint: Hint{Intent: IntentCheap},
			want: specD,
		},
		{
			name: "step 5 terminal fallback to default",
			hint: Hint{Intent: Intent("unknown")},
			want: specE,
		},
		{
			name: "empty intent treated as default",
			hint: Hint{},
			want: specE,
		},
	}
	r := newRouter(t, defaults)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models := r.Defaults()
			_ = models
			// register the override-target if not already in router
			if tc.hint.ModelOverride != nil {
				r.models[*tc.hint.ModelOverride] = &fakeModel{spec: *tc.hint.ModelOverride}
			}
			for _, s := range tc.hint.SessionModels {
				r.models[s] = &fakeModel{spec: s}
			}
			for _, s := range tc.hint.SkillModels {
				r.models[s] = &fakeModel{spec: s}
			}
			got, spec, err := r.Resolve(context.Background(), tc.hint)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if spec != tc.want {
				t.Errorf("spec: got %s, want %s", spec, tc.want)
			}
			if got.Spec() != tc.want {
				t.Errorf("model.Spec: got %s, want %s", got.Spec(), tc.want)
			}
		})
	}
}

func TestRouter_UnregisteredOverride(t *testing.T) {
	defaults := map[Intent]ModelSpec{
		IntentDefault: {Provider: "p", Name: "default"},
	}
	r := newRouter(t, defaults)
	bogus := ModelSpec{Provider: "p", Name: "bogus"}
	_, _, err := r.Resolve(context.Background(), Hint{ModelOverride: &bogus})
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("expected ErrModelUnavailable, got %v", err)
	}
}

func TestRouter_MissingDefault(t *testing.T) {
	_, err := NewModelRouter(map[Intent]ModelSpec{
		IntentCheap: {Provider: "p", Name: "x"},
	}, map[ModelSpec]Model{
		{Provider: "p", Name: "x"}: &fakeModel{spec: ModelSpec{Provider: "p", Name: "x"}},
	})
	if !errors.Is(err, ErrModelMisconfigured) {
		t.Fatalf("expected ErrModelMisconfigured, got %v", err)
	}
}

func TestRouter_SpecReferencedButNotRegistered(t *testing.T) {
	defaults := map[Intent]ModelSpec{
		IntentDefault: {Provider: "p", Name: "default"},
	}
	_, err := NewModelRouter(defaults, map[ModelSpec]Model{})
	if !errors.Is(err, ErrModelMisconfigured) {
		t.Fatalf("expected ErrModelMisconfigured, got %v", err)
	}
}

func TestRouter_PrecedenceStack(t *testing.T) {
	specOverride := ModelSpec{Provider: "p", Name: "override"}
	specSession := ModelSpec{Provider: "p", Name: "session"}
	specSkill := ModelSpec{Provider: "p", Name: "skill"}
	specRuntime := ModelSpec{Provider: "p", Name: "runtime"}
	specFallback := ModelSpec{Provider: "p", Name: "fallback"}

	defaults := map[Intent]ModelSpec{
		IntentDefault: specFallback,
		Intent("X"):   specRuntime,
	}
	r := newRouter(t, defaults)
	// register the layered candidates
	for _, s := range []ModelSpec{specOverride, specSession, specSkill} {
		r.models[s] = &fakeModel{spec: s}
	}

	// All four lower-priority sources present; override must still win.
	hint := Hint{
		Intent:        Intent("X"),
		ModelOverride: &specOverride,
		SessionModels: map[Intent]ModelSpec{Intent("X"): specSession},
		SkillModels:   map[Intent]ModelSpec{Intent("X"): specSkill},
	}
	_, spec, err := r.Resolve(context.Background(), hint)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec != specOverride {
		t.Fatalf("override must win: got %s", spec)
	}
}
