package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateOne_HappyPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(`---
name: ok-skill
description: ok
license: MIT
---
`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, code := validateOne(dir)
	if code != exitOK {
		t.Errorf("code = %d, want %d", code, exitOK)
	}
	if !r.OK {
		t.Errorf("r.OK = false, reason: %s", r.Reason)
	}
	if r.Name != "ok-skill" {
		t.Errorf("r.Name = %q", r.Name)
	}
}

func TestValidateOne_DirectFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(`---
name: ok
description: ok
license: MIT
---
`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, code := validateOne(path)
	if code != exitOK || !r.OK {
		t.Errorf("validateOne file path: code=%d r=%+v", code, r)
	}
}

func TestValidateOne_InvalidManifestExitsOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(`---
name: with space
description: bad name
license: MIT
---
`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, code := validateOne(dir)
	if code != exitInvalid {
		t.Errorf("code = %d, want %d", code, exitInvalid)
	}
	if r.OK {
		t.Errorf("r.OK = true on invalid manifest")
	}
	if !strings.Contains(r.Reason, "name") {
		t.Errorf("r.Reason missing name diagnostic: %q", r.Reason)
	}
}

func TestValidateOne_MissingPathExitsTwo(t *testing.T) {
	r, code := validateOne(filepath.Join(t.TempDir(), "no-such"))
	if code != exitIO {
		t.Errorf("code = %d, want %d", code, exitIO)
	}
	if r.OK {
		t.Errorf("r.OK = true on missing path")
	}
}

func TestValidateOne_DirWithoutSKILLmdExitsTwo(t *testing.T) {
	dir := t.TempDir() // empty dir
	r, code := validateOne(dir)
	if code != exitIO {
		t.Errorf("code = %d, want %d", code, exitIO)
	}
	if r.OK {
		t.Errorf("r.OK = true on dir without SKILL.md")
	}
}
