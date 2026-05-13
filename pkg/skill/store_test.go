package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestStore_DirBackend_ListAndGet(t *testing.T) {
	root := t.TempDir()
	mustWriteSkill(t, root, "alpha", `---
name: alpha
description: First skill.
license: MIT
---
# Alpha
`)
	mustWriteSkill(t, root, "beta", `---
name: beta
description: Second skill.
license: MIT
---
# Beta
`)

	s := NewSkillStore(Options{HubRoot: root})

	listed, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("List len = %d, want 2", len(listed))
	}

	got, err := s.Get(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Manifest.Name != "alpha" {
		t.Errorf("Get returned %q, want alpha", got.Manifest.Name)
	}
	if got.Origin != OriginHub {
		t.Errorf("Get returned origin %v, want hub", got.Origin)
	}
	if got.FS == nil {
		t.Errorf("Get returned nil FS")
	}
}

func TestStore_GetUnknownSkill(t *testing.T) {
	s := NewSkillStore(Options{HubRoot: t.TempDir()})
	_, err := s.Get(context.Background(), "missing")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("err = %v, want ErrSkillNotFound", err)
	}
}

func TestStore_ShadowingOrder_HubOverLocal(t *testing.T) {
	hubRoot := t.TempDir()
	localRoot := t.TempDir()
	mustWriteSkill(t, hubRoot, "shared", `---
name: shared
description: From hub.
license: MIT
---
# hub body`)
	mustWriteSkill(t, localRoot, "shared", `---
name: shared
description: From local.
license: MIT
---
# local body`)

	s := NewSkillStore(Options{
		HubRoot:   hubRoot,
		LocalRoot: localRoot,
	})

	got, err := s.Get(context.Background(), "shared")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Origin != OriginHub {
		t.Errorf("Origin = %v, want hub (hub shadows local)", got.Origin)
	}
	if got.Manifest.Description != "From hub." {
		t.Errorf("Description = %q, want From hub.", got.Manifest.Description)
	}
}

func TestStore_InlineBackend(t *testing.T) {
	s := NewSkillStore(Options{
		Inline: map[string][]byte{
			"inline-skill": []byte(`---
name: inline-skill
description: Inline.
license: MIT
---
`),
		},
	})

	got, err := s.Get(context.Background(), "inline-skill")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Origin != OriginInline {
		t.Errorf("Origin = %v, want inline", got.Origin)
	}
}

func TestStore_HubStub_GetMisses(t *testing.T) {
	s := NewSkillStore(Options{}) // hub-only by default
	_, err := s.Get(context.Background(), "anything")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("err = %v, want ErrSkillNotFound", err)
	}
}

func TestStore_PublishLocal(t *testing.T) {
	localRoot := t.TempDir()
	s := NewSkillStore(Options{LocalRoot: localRoot})

	manifest, err := Parse([]byte(`---
name: published
description: Published locally.
license: MIT
---
# Published
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	body := fstest.MapFS{
		"references/extra.md": &fstest.MapFile{Data: []byte("extra reference")},
	}

	if err := s.Publish(context.Background(), manifest, body, PublishOptions{}); err != nil {
		t.Fatalf("Publish error: %v", err)
	}

	// Round-trip: Get the same skill back.
	got, err := s.Get(context.Background(), "published")
	if err != nil {
		t.Fatalf("Get after Publish error: %v", err)
	}
	if got.Manifest.Name != "published" {
		t.Errorf("Name = %q, want published", got.Manifest.Name)
	}
	if got.Origin != OriginLocal {
		t.Errorf("Origin = %v, want local", got.Origin)
	}

	// Body file should exist.
	extra := filepath.Join(localRoot, "published", "references", "extra.md")
	data, err := os.ReadFile(extra)
	if err != nil {
		t.Fatalf("read body file error: %v", err)
	}
	if string(data) != "extra reference" {
		t.Errorf("body content = %q", string(data))
	}
}

func TestStore_PublishUnsupportedWhenNoLocalRoot(t *testing.T) {
	s := NewSkillStore(Options{HubRoot: t.TempDir()})
	_, err := Parse([]byte(`---
name: x
description: y
license: MIT
---
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	manifest, _ := Parse([]byte("---\nname: x\ndescription: y\nlicense: MIT\n---\n"))
	err = s.Publish(context.Background(), manifest, nil, PublishOptions{})
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Fatalf("err = %v, want ErrUnsupportedBackend", err)
	}
}

func TestStore_BadSkillReportedButOthersListed(t *testing.T) {
	root := t.TempDir()
	mustWriteSkill(t, root, "good", `---
name: good
description: ok
license: MIT
---
`)
	// Broken: invalid YAML.
	bad := filepath.Join(root, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatalf("mkdir bad: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("---\nname: [list, not, string]\n---\n"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	s := NewSkillStore(Options{HubRoot: root})
	listed, err := s.List(context.Background())
	if err == nil {
		t.Fatal("List error = nil, want partial-failure error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want to wrap ErrManifestInvalid", err)
	}
	if len(listed) != 1 || listed[0].Manifest.Name != "good" {
		t.Errorf("listed = %+v, want [good]", listed)
	}
}

// TestStore_PublishCollision verifies that a second Publish under
// the same name without overwrite returns ErrSkillExists, and that
// overwrite=true replaces the existing bundle (removing files that
// were present in the previous version but not in the new bundle).
func TestStore_PublishCollision(t *testing.T) {
	localRoot := t.TempDir()
	s := NewSkillStore(Options{LocalRoot: localRoot})

	manifest, err := Parse([]byte(`---
name: collision
description: collision-test skill.
license: MIT
---
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	v1 := fstest.MapFS{
		"references/v1.md": &fstest.MapFile{Data: []byte("v1 contents")},
	}
	if err := s.Publish(context.Background(), manifest, v1, PublishOptions{}); err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	// Second publish without overwrite must fail.
	v2 := fstest.MapFS{
		"references/v2.md": &fstest.MapFile{Data: []byte("v2 contents")},
	}
	err = s.Publish(context.Background(), manifest, v2, PublishOptions{Overwrite: false})
	if !errors.Is(err, ErrSkillExists) {
		t.Fatalf("second Publish err = %v, want ErrSkillExists", err)
	}

	// Existing v1 file must still be there.
	v1Path := filepath.Join(localRoot, "collision", "references", "v1.md")
	if _, err := os.Stat(v1Path); err != nil {
		t.Errorf("v1 file gone after rejected overwrite: %v", err)
	}

	// Third publish WITH overwrite must succeed and replace contents.
	if err := s.Publish(context.Background(), manifest, v2, PublishOptions{Overwrite: true}); err != nil {
		t.Fatalf("overwrite Publish: %v", err)
	}

	// v1 file should be gone (full directory replacement).
	if _, err := os.Stat(v1Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("v1 file lingered after overwrite: stat err = %v", err)
	}
	// v2 file should be present.
	v2Path := filepath.Join(localRoot, "collision", "references", "v2.md")
	if _, err := os.Stat(v2Path); err != nil {
		t.Errorf("v2 file missing after overwrite: %v", err)
	}
}

// TestCleanRelPath_RejectsUnsafeKeys covers the path-safety rules
// skill:save uses to validate references/scripts/assets keys.
func TestCleanRelPath_RejectsUnsafeKeys(t *testing.T) {
	bad := []string{
		"",                  // empty
		"/etc/passwd",       // absolute
		"../escape.md",      // parent-dir
		"foo/../bar.md",     // parent-dir mid-path (also non-normalised)
		"./foo.md",          // hidden via leading dot (also non-normalised)
		".env",              // hidden segment
		"foo/.hidden/bar",   // hidden mid-path
		"foo\x00.md",        // NUL byte
		"foo\\bar.md",       // backslash
		"foo//bar.md",       // double slash (non-normalised)
		"foo/",              // trailing slash (non-normalised)
		".",                 // pure dot
		"..",                // pure parent
	}
	for _, p := range bad {
		if _, err := CleanRelPath(p); err == nil {
			t.Errorf("CleanRelPath(%q) accepted, want ErrInvalidPath", p)
		} else if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanRelPath(%q) err = %v, want ErrInvalidPath wrap", p, err)
		}
	}

	good := []string{
		"foo.md",
		"sub/foo.py",
		"sub/dir/file.json",
		"a-b_c.md",
	}
	for _, p := range good {
		got, err := CleanRelPath(p)
		if err != nil {
			t.Errorf("CleanRelPath(%q) err = %v, want nil", p, err)
		}
		if got != p {
			t.Errorf("CleanRelPath(%q) = %q, want unchanged", p, got)
		}
	}
}

func mustWriteSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
