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

	s := NewSkillStore(Options{SystemRoot: root})

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
	if got.Origin != OriginSystem {
		t.Errorf("Get returned origin %v, want system", got.Origin)
	}
	if got.FS == nil {
		t.Errorf("Get returned nil FS")
	}
}

func TestStore_GetUnknownSkill(t *testing.T) {
	s := NewSkillStore(Options{SystemRoot: t.TempDir()})
	_, err := s.Get(context.Background(), "missing")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("err = %v, want ErrSkillNotFound", err)
	}
}

func TestStore_ShadowingOrder_SystemOverLocal(t *testing.T) {
	systemRoot := t.TempDir()
	localRoot := t.TempDir()
	mustWriteSkill(t, systemRoot, "shared", `---
name: shared
description: From system.
license: MIT
---
# system body`)
	mustWriteSkill(t, localRoot, "shared", `---
name: shared
description: From local.
license: MIT
---
# local body`)

	s := NewSkillStore(Options{
		SystemRoot: systemRoot,
		LocalRoot:  localRoot,
	})

	got, err := s.Get(context.Background(), "shared")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Origin != OriginSystem {
		t.Errorf("Origin = %v, want system (system shadows local)", got.Origin)
	}
	if got.Manifest.Description != "From system." {
		t.Errorf("Description = %q, want From system.", got.Manifest.Description)
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

	if err := s.Publish(context.Background(), manifest, body); err != nil {
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
	s := NewSkillStore(Options{SystemRoot: t.TempDir()})
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
	err = s.Publish(context.Background(), manifest, nil)
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

	s := NewSkillStore(Options{SystemRoot: root})
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
