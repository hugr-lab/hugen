package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// minUVMajor / minUVMinor / minUVPatch — uv >= 0.4.0. Earlier
// builds had bugs around `--relocatable` symlink rewriting; 0.4.0
// is the conservative floor (see specs research §R3).
const (
	minUVMajor = 0
	minUVMinor = 4
	minUVPatch = 0
)

// stampName is the file written into the template output dir
// (and into per-session venv copies) marking a successful build /
// copy. Existence of the stamp is the only "ready" signal.
const stampName = ".bootstrap-complete"

// BuildTemplate builds a relocatable Python venv at outDir from
// reqsPath. Idempotent: a previous build whose stamp is newer than
// the requirements file returns nil immediately (SC-007).
func BuildTemplate(ctx context.Context, reqsPath, outDir string, log *slog.Logger) error {
	if err := assertUV(ctx); err != nil {
		return err
	}
	stamp := filepath.Join(outDir, stampName)
	if upToDate, err := stampUpToDate(stamp, reqsPath); err != nil {
		return err
	} else if upToDate {
		log.Info("python-mcp: template up to date", "out", outDir)
		return nil
	}

	if err := os.RemoveAll(outDir); err != nil {
		return fmt.Errorf("python-mcp: rm %s: %w", outDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(outDir), 0o755); err != nil {
		return fmt.Errorf("python-mcp: mkdir parent: %w", err)
	}

	log.Info("python-mcp: building venv", "out", outDir)
	venv := exec.CommandContext(ctx, "uv", "venv", "--relocatable", outDir)
	venv.Stdout = os.Stdout
	venv.Stderr = os.Stderr
	if err := venv.Run(); err != nil {
		return fmt.Errorf("python-mcp: uv venv: %w", err)
	}

	log.Info("python-mcp: installing requirements", "reqs", reqsPath)
	pip := exec.CommandContext(ctx, "uv", "pip", "install",
		"--python", filepath.Join(outDir, "bin", "python"),
		"-r", reqsPath)
	pip.Stdout = os.Stdout
	pip.Stderr = os.Stderr
	if err := pip.Run(); err != nil {
		return fmt.Errorf("python-mcp: uv pip install: %w", err)
	}

	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		return fmt.Errorf("python-mcp: write stamp: %w", err)
	}
	log.Info("python-mcp: template ready", "out", outDir)
	return nil
}

// assertUV checks that `uv --version` reports >= 0.4.0. Operators
// who skipped the README install step get the floor named in the
// error message rather than a confusing pip / venv failure later.
func assertUV(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "uv", "--version").Output()
	if err != nil {
		return fmt.Errorf("python-mcp: uv not on PATH (need >= %d.%d.%d): %w",
			minUVMajor, minUVMinor, minUVPatch, err)
	}
	maj, min, patch, ok := parseUVVersion(string(out))
	if !ok {
		return fmt.Errorf("python-mcp: cannot parse uv --version output: %q", string(out))
	}
	if maj < minUVMajor ||
		(maj == minUVMajor && min < minUVMinor) ||
		(maj == minUVMajor && min == minUVMinor && patch < minUVPatch) {
		return fmt.Errorf("python-mcp: uv %d.%d.%d < required %d.%d.%d",
			maj, min, patch, minUVMajor, minUVMinor, minUVPatch)
	}
	return nil
}

// parseUVVersion extracts (major, minor, patch) from a `uv X.Y.Z` line.
// Returns ok=false on any unexpected shape — caller produces a clear
// error rather than silently accepting weird builds.
func parseUVVersion(s string) (maj, min, patch int, ok bool) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) < 2 || fields[0] != "uv" {
		return 0, 0, 0, false
	}
	parts := strings.Split(fields[1], ".")
	if len(parts) < 3 {
		return 0, 0, 0, false
	}
	var err error
	maj, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	min, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	patch, err = strconv.Atoi(stripNonDigits(parts[2]))
	if err != nil {
		return 0, 0, 0, false
	}
	return maj, min, patch, true
}

func stripNonDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}

// stampUpToDate returns true when the template's stamp file is
// present AND newer than the requirements file. Anything else
// (no stamp / older stamp / read error) yields false.
func stampUpToDate(stamp, reqsPath string) (bool, error) {
	stampInfo, err := os.Stat(stamp)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("python-mcp: stat stamp: %w", err)
	}
	reqsInfo, err := os.Stat(reqsPath)
	if err != nil {
		return false, fmt.Errorf("python-mcp: stat requirements: %w", err)
	}
	return stampInfo.ModTime().After(reqsInfo.ModTime()), nil
}
