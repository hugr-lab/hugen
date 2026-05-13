package runtime

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/session"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// TestReadConstitutionEmbed_Universal verifies the universal
// preamble is readable straight from the embedded bundle — the
// only resolution path after the embed-only refresh fix.
func TestReadConstitutionEmbed_Universal(t *testing.T) {
	body, err := readConstitutionEmbed(constitutionDefaultFile)
	if err != nil {
		t.Fatalf("read embed: %v", err)
	}
	embed, err := fs.ReadFile(assets.ConstitutionFS,
		filepath.Join(constitutionEmbedRoot, constitutionDefaultFile))
	if err != nil {
		t.Fatalf("direct read: %v", err)
	}
	if body != string(embed) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(body), len(embed))
	}
}

func TestReadConstitutionEmbed_MissingReturnsNotExist(t *testing.T) {
	_, err := readConstitutionEmbed("nonexistent-file-x.md")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got %v", err)
	}
}

// TestLoadConstitutionBundle_LoadsEveryTier asserts the live
// bundle exposes the universal preamble plus every tier manual
// shipped under assets/constitution/. Acts as a backstop against
// accidental tier-file deletions in the bundle.
func TestLoadConstitutionBundle_LoadsEveryTier(t *testing.T) {
	universal, manuals, err := loadConstitutionBundle()
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	if universal == "" {
		t.Errorf("universal preamble empty")
	}
	for _, tier := range []string{
		skillpkg.TierRoot,
		skillpkg.TierMission,
		skillpkg.TierWorker,
	} {
		if manuals[tier] == "" {
			t.Errorf("tier %q manual missing or empty", tier)
		}
	}
}

func TestRegisterBuiltinCommands(t *testing.T) {
	reg := session.NewCommandRegistry()
	if err := RegisterBuiltinCommands(reg, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	want := []string{"cancel", "end", "help", "model"}
	got := reg.Names()
	if len(got) != len(want) {
		t.Fatalf("expected %d commands, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Re-registration must fail (already registered).
	if err := RegisterBuiltinCommands(reg, nil); err == nil {
		t.Fatal("re-register expected to fail")
	}
}
