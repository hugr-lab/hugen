package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// --- safeExtractTarGz: the security-critical path ---

func TestSafeExtractTarGz_Normal(t *testing.T) {
	dst := t.TempDir()
	data := buildTarGz(t, map[string]string{
		"SKILL.md":         "name: x\n",
		"scripts/run.py":   "print(1)\n",
		"nested/a/b/c.txt": "deep",
	})
	if err := safeExtractTarGz(bytes.NewReader(data), dst, maxBundleBytes, maxBundleFiles); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, p := range []string{"SKILL.md", "scripts/run.py", "nested/a/b/c.txt"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(p))); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestSafeExtractTarGz_RejectsTraversal(t *testing.T) {
	cases := map[string][]string{
		"dotdot":   {"../escape.txt"},
		"absolute": {"/etc/evil"},
		"deep":     {"a/../../escape"},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			for _, e := range entries {
				body := []byte("x")
				_ = tw.WriteHeader(&tar.Header{Name: e, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
				_, _ = tw.Write(body)
			}
			_ = tw.Close()
			_ = gz.Close()
			if err := safeExtractTarGz(&buf, t.TempDir(), maxBundleBytes, maxBundleFiles); err == nil {
				t.Errorf("%s: extraction accepted an escaping path", name)
			}
		})
	}
}

func TestSafeExtractTarGz_RejectsSymlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.Close()
	_ = gz.Close()
	if err := safeExtractTarGz(&buf, t.TempDir(), maxBundleBytes, maxBundleFiles); err == nil {
		t.Fatal("symlink entry accepted")
	}
}

func TestSafeExtractTarGz_SizeCap(t *testing.T) {
	data := buildTarGz(t, map[string]string{"big.txt": strings.Repeat("A", 4096)})
	if err := safeExtractTarGz(bytes.NewReader(data), t.TempDir(), 1024, maxBundleFiles); err == nil {
		t.Fatal("size cap not enforced")
	}
}

func TestSafeExtractTarGz_FileCountCap(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("f%d.txt", i)] = "x"
	}
	data := buildTarGz(t, files)
	if err := safeExtractTarGz(bytes.NewReader(data), t.TempDir(), maxBundleBytes, 3); err == nil {
		t.Fatal("file-count cap not enforced")
	}
}

// --- decideFetch: origin preservation + additive-only ---

func TestDecideFetch(t *testing.T) {
	dir := t.TempDir()
	ledger, _ := skill.LoadLedger(dir)
	ledger.Set("seeded", skill.LedgerEntry{Hash: "sha256:a", Origin: skill.InstallSeed})
	ledger.Set("selfed", skill.LedgerEntry{Hash: "sha256:b", Origin: skill.InstallSelf})

	// Existing entries upgrade in place with origin preserved.
	if o, ok := decideFetch("seeded", ledger, true, map[string]struct{}{}); !ok || o != skill.InstallSeed {
		t.Errorf("seeded: got (%v,%v), want (seed,true)", o, ok)
	}
	if o, ok := decideFetch("selfed", ledger, false, nil); !ok || o != skill.InstallSelf {
		t.Errorf("selfed: got (%v,%v), want (self,true)", o, ok)
	}
	// A new name is fetched only when declared in the install-set (→ desired).
	if o, ok := decideFetch("wanted", ledger, true, map[string]struct{}{"wanted": {}}); !ok || o != skill.InstallDesired {
		t.Errorf("wanted: got (%v,%v), want (desired,true)", o, ok)
	}
	// A catalog-only name with no ledger entry and no declared request is NOT
	// fetched — the reconciler is additive to the seed baseline.
	if _, ok := decideFetch("stranger", ledger, true, map[string]struct{}{"wanted": {}}); ok {
		t.Error("stranger fetched despite not being declared or seeded")
	}
	if _, ok := decideFetch("stranger", ledger, false, nil); ok {
		t.Error("stranger fetched in undeclared (OOTB) mode")
	}
}

// --- reconcileOnce: end-to-end against an httptest marketplace ---

func TestReconcileOnce_DownloadsExtractsAndLedgers(t *testing.T) {
	files := map[string]string{"SKILL.md": "name: demo\n", "scripts/run.py": "print(1)\n"}
	hash := bundleHashOf(t, files)
	srv := marketplaceServer(t, map[string]bundleFixture{"demo": {files: files, hash: hash}})
	defer srv.Close()

	state := t.TempDir()
	hubDir := filepath.Join(state, "skills/hub")
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newTestReconciler(srv, hubDir, stubSkillsView{install: []string{"demo"}, declared: true})

	if _, err := r.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hubDir, "demo", "SKILL.md")); err != nil {
		t.Errorf("bundle not materialised: %v", err)
	}
	ledger, _ := skill.LoadLedger(hubDir)
	entry, ok := ledger.Get("demo")
	if !ok {
		t.Fatal("ledger missing demo")
	}
	if entry.Origin != skill.InstallDesired || entry.Hash != hash {
		t.Errorf("ledger entry = %+v, want desired @ %s", entry, hash)
	}
}

func TestReconcileOnce_UpgradePreservesSeedOrigin(t *testing.T) {
	v2 := map[string]string{"SKILL.md": "name: demo\n", "body": "v2"}
	v2hash := bundleHashOf(t, v2)
	srv := marketplaceServer(t, map[string]bundleFixture{"demo": {files: v2, hash: v2hash}})
	defer srv.Close()

	state := t.TempDir()
	hubDir := filepath.Join(state, "skills/hub")
	if err := os.MkdirAll(filepath.Join(hubDir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing seed at an old hash.
	ledger, _ := skill.LoadLedger(hubDir)
	ledger.Set("demo", skill.LedgerEntry{Hash: "sha256:old", Origin: skill.InstallSeed})
	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}
	// Not declared: the upgrade must still happen because the skill is already
	// in the ledger (existing installs upgrade in place).
	r := newTestReconciler(srv, hubDir, stubSkillsView{declared: false})
	if _, err := r.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after, _ := skill.LoadLedger(hubDir)
	entry, _ := after.Get("demo")
	if entry.Hash != v2hash {
		t.Errorf("not upgraded: hash %s, want %s", entry.Hash, v2hash)
	}
	if entry.Origin != skill.InstallSeed {
		t.Errorf("origin flipped to %s, want seed (preserved)", entry.Origin)
	}
	if _, err := os.Stat(filepath.Join(hubDir, "demo", "body")); err != nil {
		t.Errorf("upgraded body not written: %v", err)
	}
}

func TestReconcileOnce_RejectsHashMismatch(t *testing.T) {
	files := map[string]string{"SKILL.md": "name: demo\n"}
	// Catalog advertises a WRONG hash → the extracted content must be rejected.
	srv := marketplaceServer(t, map[string]bundleFixture{"demo": {files: files, hash: "sha256:deadbeef"}})
	defer srv.Close()

	state := t.TempDir()
	hubDir := filepath.Join(state, "skills/hub")
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newTestReconciler(srv, hubDir, stubSkillsView{install: []string{"demo"}, declared: true})
	if _, err := r.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile returned error (should be per-skill best-effort): %v", err)
	}
	// The mismatched bundle must NOT be installed or ledgered.
	if _, err := os.Stat(filepath.Join(hubDir, "demo")); err == nil {
		t.Error("mismatched bundle was installed")
	}
	ledger, _ := skill.LoadLedger(hubDir)
	if _, ok := ledger.Get("demo"); ok {
		t.Error("mismatched bundle was ledgered")
	}
}

// --- Install / Refresh: the SK4 on-demand marketplace ops ---

func TestReconciler_Install(t *testing.T) {
	files := map[string]string{"SKILL.md": "name: demo\n", "scripts/run.py": "print(1)\n"}
	hash := bundleHashOf(t, files)
	srv := marketplaceServer(t, map[string]bundleFixture{"demo": {files: files, hash: hash}})
	defer srv.Close()

	state := t.TempDir()
	hubDir := filepath.Join(state, "skills/hub")
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Not declared in any install-set: skill:install is an explicit user pull,
	// independent of the download policy.
	r := newTestReconciler(srv, hubDir, stubSkillsView{declared: false})

	out, err := r.Install(context.Background(), "demo")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if out.Name != "demo" || out.ContentHash != hash || out.AlreadyCurrent {
		t.Errorf("outcome = %+v, want demo @ %s (not already-current)", out, hash)
	}
	if _, err := os.Stat(filepath.Join(hubDir, "demo", "SKILL.md")); err != nil {
		t.Errorf("bundle not materialised: %v", err)
	}
	// A user-initiated install lands as origin=self.
	ledger, _ := skill.LoadLedger(hubDir)
	if e, ok := ledger.Get("demo"); !ok || e.Origin != skill.InstallSelf {
		t.Errorf("ledger origin = %+v ok=%v, want self", e, ok)
	}

	// Re-installing the same content is a no-op (AlreadyCurrent).
	out2, err := r.Install(context.Background(), "demo")
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if !out2.AlreadyCurrent {
		t.Errorf("re-install not reported already-current: %+v", out2)
	}

	// An unknown name is a clear error, not a silent no-op.
	if _, err := r.Install(context.Background(), "nope"); err == nil {
		t.Error("install of an uncatalogued skill returned no error")
	}
}

func TestReconciler_Refresh(t *testing.T) {
	files := map[string]string{"SKILL.md": "name: demo\n"}
	hash := bundleHashOf(t, files)
	srv := marketplaceServer(t, map[string]bundleFixture{"demo": {files: files, hash: hash}})
	defer srv.Close()

	state := t.TempDir()
	hubDir := filepath.Join(state, "skills/hub")
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newTestReconciler(srv, hubDir, stubSkillsView{install: []string{"demo"}, declared: true})

	out, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if out.Downloaded != 1 || out.Failed != 0 {
		t.Errorf("refresh outcome = %+v, want 1 downloaded / 0 failed", out)
	}
}

// --- test helpers ---

func newTestReconciler(srv *httptest.Server, hubDir string, view stubSkillsView) *skillReconciler {
	return &skillReconciler{
		hubURL:  srv.URL,
		hubDir:  hubDir,
		client:  srv.Client(),
		store:   skill.NewSkillStore(skill.Options{}), // no dynamic → IndexHubBundles no-ops
		skills:  view,
		refresh: 0,
		log:     discardLogger(),
		trigger: make(chan struct{}, 1),
	}
}

type bundleFixture struct {
	files map[string]string
	hash  string
}

func marketplaceServer(t *testing.T, fixtures map[string]bundleFixture) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/skills/catalog", func(w http.ResponseWriter, req *http.Request) {
		var out struct {
			Skills []catalogEntry `json:"skills"`
		}
		for name, f := range fixtures {
			out.Skills = append(out.Skills, catalogEntry{Name: name, Version: "v1", ContentHash: f.hash})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/skills/", func(w http.ResponseWriter, req *http.Request) {
		// /skills/{name}/bundle
		name := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/skills/"), "/bundle")
		f, ok := fixtures[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("X-Skill-Content-Hash", f.hash)
		_, _ = w.Write(buildTarGzMap(f.files))
	})
	return httptest.NewServer(mux)
}

// buildTarGz builds a gzip-compressed tar of files (path → content), matching
// the hub seeder's dotfile-excluding layout.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	return buildTarGzMap(files)
}

func buildTarGzMap(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for p, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// bundleHashOf computes the canonical BundleHash of a file map — the value the
// marketplace's catalog would advertise for that content.
func bundleHashOf(t *testing.T, files map[string]string) string {
	t.Helper()
	m := fstest.MapFS{}
	for p, content := range files {
		m[p] = &fstest.MapFile{Data: []byte(content)}
	}
	h, err := skill.BundleHash(m)
	if err != nil {
		t.Fatalf("bundle hash: %v", err)
	}
	return h
}

// stubSkillsView is a minimal config.SkillsView for reconciler tests.
type stubSkillsView struct {
	install  []string
	declared bool
}

func (s stubSkillsView) InstallSet() []string         { return s.install }
func (s stubSkillsView) InstallSetDeclared() bool     { return s.declared }
func (s stubSkillsView) PinSet() []string             { return nil }
func (s stubSkillsView) PinSetDeclared() bool         { return false }
func (s stubSkillsView) OnUpdate(func()) (cancel func()) { return func() {} }
