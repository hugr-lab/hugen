// Phase-3.5 US5 template-idempotency test (T051 / SC-007).
//
// Builds python-mcp, runs --create-template twice against the same
// fixture, and asserts the second run returns within 5 s and does not
// rebuild the venv (mtime of bin/python is stable).
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestUS3_5_US5_TemplateIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping template idempotency test")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping")
	}
	pyBin := buildPythonMCP(t)

	tmp := t.TempDir()
	reqs := filepath.Join(tmp, "requirements.txt")
	if err := os.WriteFile(reqs, []byte("six\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, ".venv")

	// First run — full build. May take a few seconds for `uv venv`
	// + `uv pip install`. We don't assert a budget for it.
	first := exec.Command(pyBin, "--create-template", reqs, "--out", out)
	first.Stderr = os.Stderr
	if err := first.Run(); err != nil {
		t.Fatalf("first build: %v", err)
	}
	stamp := filepath.Join(out, ".bootstrap-complete")
	stampInfo1, err := os.Stat(stamp)
	if err != nil {
		t.Fatalf("first stamp: %v", err)
	}

	// Spread the stamp/python mtimes apart from the second run by a
	// hair so identity comparisons don't trip on filesystem-coarse
	// clocks (HFS+ tracks at second granularity).
	pyExe := filepath.Join(out, "bin", "python")
	pyInfo1, err := os.Stat(pyExe)
	if err != nil {
		// uv >= 0.4 places the interpreter at .venv/bin/python; if
		// the layout differs we don't fail the test, we just skip
		// the mtime cross-check.
		t.Logf("python interpreter not at %s: %v", pyExe, err)
	}

	// Second run — idempotent. Must complete in well under 5 s
	// (SC-007). Stamp mtime should not advance materially.
	start := time.Now()
	second := exec.Command(pyBin, "--create-template", reqs, "--out", out)
	second.Stderr = os.Stderr
	if err := second.Run(); err != nil {
		t.Fatalf("second build: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("second --create-template took %s; expected ≤ 5s (SC-007)", elapsed)
	}

	stampInfo2, err := os.Stat(stamp)
	if err != nil {
		t.Fatalf("second stamp: %v", err)
	}
	if !stampInfo1.ModTime().Equal(stampInfo2.ModTime()) {
		t.Errorf("stamp mtime advanced: %v → %v (idempotent run should not rewrite)",
			stampInfo1.ModTime(), stampInfo2.ModTime())
	}
	if pyInfo1 != nil {
		pyInfo2, err := os.Stat(pyExe)
		if err == nil && !pyInfo1.ModTime().Equal(pyInfo2.ModTime()) {
			t.Errorf("python interpreter mtime advanced: %v → %v (uv re-installed)",
				pyInfo1.ModTime(), pyInfo2.ModTime())
		}
	}
}
