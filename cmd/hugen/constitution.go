package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hugr-lab/hugen/assets"
)

// constitutionSubdir is the directory under StateDir where the
// agent constitution materialises. Operators override the bundled
// copy by editing files in this directory; if a file exists at
// boot, it shadows the embedded one.
const constitutionSubdir = "constitution"

// constitutionDefaultFile is the universal-rules markdown.
const constitutionDefaultFile = "agent.md"

// loadConstitution returns the agent's constitution markdown body.
// Search order:
//  1. ${stateDir}/constitution/agent.md — operator override.
//  2. assets/constitution/agent.md — bundled default.
//
// On first boot the bundled copy is also materialised at
// ${stateDir}/constitution/agent.md so the operator has a starting
// point to edit. Updating the binary refreshes the on-disk copy
// only when the operator hasn't customised it (file matches
// embedded byte-for-byte) — otherwise the operator's edits stay.
func loadConstitution(stateDir string, log *slog.Logger) (string, error) {
	if stateDir == "" {
		return "", errors.New("constitution: empty state dir")
	}
	target := filepath.Join(stateDir, constitutionSubdir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", fmt.Errorf("constitution: mkdir: %w", err)
	}

	embedded, err := fs.ReadFile(assets.ConstitutionFS, filepath.Join("constitution", constitutionDefaultFile))
	if err != nil {
		return "", fmt.Errorf("constitution: read embed: %w", err)
	}

	disk := filepath.Join(target, constitutionDefaultFile)
	current, err := os.ReadFile(disk)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.WriteFile(disk, embedded, 0o644); err != nil {
			return "", fmt.Errorf("constitution: write default: %w", err)
		}
		log.Info("constitution materialised", "path", disk)
		return string(embedded), nil
	case err != nil:
		return "", fmt.Errorf("constitution: read disk: %w", err)
	}

	// Operator may have edited the on-disk copy — preserve it.
	// Treat operator's bytes as authoritative; the embedded copy
	// is only a starting template.
	return string(current), nil
}
