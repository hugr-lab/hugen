package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// fakeSkills is a stub skillManager for handler tests.
type fakeSkills struct {
	list       []skill.Skill
	getFn      func(name string) (skill.Skill, error)
	publishErr error
	published  *publishCall
}

type publishCall struct {
	manifest skill.Manifest
	body     fs.FS
	opts     skill.PublishOptions
}

func (f *fakeSkills) List(context.Context) ([]skill.Skill, error) { return f.list, nil }
func (f *fakeSkills) Get(_ context.Context, name string) (skill.Skill, error) {
	return f.getFn(name)
}
func (f *fakeSkills) Publish(_ context.Context, m skill.Manifest, body fs.FS, opts skill.PublishOptions) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = &publishCall{manifest: m, body: body, opts: opts}
	return nil
}

func minimalBundle(t *testing.T, name string) []byte {
	t.Helper()
	md := "---\nname: " + name + "\ndescription: x\nlicense: MIT\n---\nBody.\n"
	tarball, err := skill.TarGzBundle(fstest.MapFS{
		"SKILL.md":          {Data: []byte(md)},
		"references/one.md": {Data: []byte("ref")},
	})
	if err != nil {
		t.Fatalf("TarGzBundle: %v", err)
	}
	return tarball
}

func TestHandleListSkills_NotConfigured(t *testing.T) {
	a := &Adapter{logger: slog.Default()} // skills nil ⇒ 501
	rec := httptest.NewRecorder()
	a.handleListSkills(rec, httptest.NewRequest(http.MethodGet, "/v1/skills", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rec.Code)
	}
}

func TestHandleListSkills_OK(t *testing.T) {
	a := &Adapter{logger: slog.Default(), skills: &fakeSkills{list: []skill.Skill{
		{Manifest: skill.Manifest{Name: "hugr-data", Description: "data"}, Origin: skill.OriginHub, FS: fstest.MapFS{}},
		{Manifest: skill.Manifest{Name: "my-local", Description: "mine"}, Origin: skill.OriginLocal, FS: fstest.MapFS{}},
	}}}
	rec := httptest.NewRecorder()
	a.handleListSkills(rec, httptest.NewRequest(http.MethodGet, "/v1/skills", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var out struct {
		Skills []skillListItem `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Skills) != 2 {
		t.Fatalf("got %d skills, want 2", len(out.Skills))
	}
	// sorted by origin: "hub" < "local"
	if out.Skills[0].Origin != "hub" || out.Skills[1].Origin != "local" {
		t.Errorf("origins = %q,%q; want hub,local", out.Skills[0].Origin, out.Skills[1].Origin)
	}
	if !out.Skills[1].Writable {
		t.Error("local skill should be writable")
	}
	if !out.Skills[0].Exportable {
		t.Error("hub skill with FS should be exportable")
	}
}

func TestHandleExportSkill_NotFound(t *testing.T) {
	a := &Adapter{logger: slog.Default(), skills: &fakeSkills{
		getFn: func(string) (skill.Skill, error) { return skill.Skill{}, skill.ErrSkillNotFound },
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/nope/export", nil)
	req.SetPathValue("name", "nope")
	rec := httptest.NewRecorder()
	a.handleExportSkill(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestHandleExportSkill_Inline(t *testing.T) {
	a := &Adapter{logger: slog.Default(), skills: &fakeSkills{
		getFn: func(string) (skill.Skill, error) {
			return skill.Skill{Manifest: skill.Manifest{Name: "x"}, Origin: skill.OriginInline}, nil // FS nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/x/export", nil)
	req.SetPathValue("name", "x")
	rec := httptest.NewRecorder()
	a.handleExportSkill(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409 (inline, no bundle)", rec.Code)
	}
}

func TestHandleExportSkill_OK(t *testing.T) {
	a := &Adapter{logger: slog.Default(), skills: &fakeSkills{
		getFn: func(string) (skill.Skill, error) {
			return skill.Skill{
				Manifest: skill.Manifest{Name: "hugr-data"},
				Origin:   skill.OriginHub,
				FS:       fstest.MapFS{"SKILL.md": {Data: []byte("---\nname: hugr-data\n---\n")}},
			}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/hugr-data/export", nil)
	req.SetPathValue("name", "hugr-data")
	rec := httptest.NewRecorder()
	a.handleExportSkill(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Error("missing Content-Disposition")
	}
	// gzip magic
	if b := rec.Body.Bytes(); len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Error("body is not a gzip stream")
	}
}

func TestHandleInstallSkill_OK(t *testing.T) {
	fake := &fakeSkills{}
	a := &Adapter{logger: slog.Default(), skills: fake}
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewReader(minimalBundle(t, "installed-skill")))
	rec := httptest.NewRecorder()
	a.handleInstallSkill(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if fake.published == nil {
		t.Fatal("Publish not called")
	}
	if fake.published.manifest.Name != "installed-skill" {
		t.Errorf("published name = %q, want installed-skill", fake.published.manifest.Name)
	}
	// body is lossless minus the root SKILL.md
	if _, err := fs.Stat(fake.published.body, "references/one.md"); err != nil {
		t.Errorf("body missing references/one.md: %v", err)
	}
	if _, err := fs.Stat(fake.published.body, "SKILL.md"); err == nil {
		t.Error("body should NOT contain the root SKILL.md (passed as manifest)")
	}
}

func TestHandleInstallSkill_Conflict(t *testing.T) {
	a := &Adapter{logger: slog.Default(), skills: &fakeSkills{publishErr: skill.ErrSkillExists}}
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewReader(minimalBundle(t, "dupe")))
	rec := httptest.NewRecorder()
	a.handleInstallSkill(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
}

func TestHandleInstallSkill_MissingManifest(t *testing.T) {
	tarball, err := skill.TarGzBundle(fstest.MapFS{"references/only.md": {Data: []byte("no manifest")}})
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{logger: slog.Default(), skills: &fakeSkills{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewReader(tarball))
	rec := httptest.NewRecorder()
	a.handleInstallSkill(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (missing SKILL.md)", rec.Code)
	}
}
