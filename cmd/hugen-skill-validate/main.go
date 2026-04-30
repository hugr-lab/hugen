// Command hugen-skill-validate parses one or more skill
// directories or SKILL.md paths and reports validity. Mirrors the
// agentskills.io skills-ref validate exit codes (0 ok, 1 invalid,
// 2 io error) and a stable JSON diagnostic format so a
// hugen-installed CI can drop in where skills-ref was used.
//
// Usage:
//
//	hugen-skill-validate <path>...
//
// Each <path> may be a SKILL.md file or a directory containing
// SKILL.md. Output is one JSON object per path, written to stdout
// in the order paths were given. The process exit code is the
// max severity across all results: 2 for any IO failure, else 1
// for any invalid manifest, else 0.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hugr-lab/hugen/pkg/skill"
)

const (
	exitOK     = 0
	exitInvalid = 1
	exitIO     = 2
)

type result struct {
	Path    string `json:"path"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Name    string `json:"name,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hugen-skill-validate <path>...")
		os.Exit(exitIO)
	}
	paths := os.Args[1:]
	rc := exitOK
	enc := json.NewEncoder(os.Stdout)
	for _, p := range paths {
		r, severity := validateOne(p)
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(os.Stderr, "encode result:", err)
			os.Exit(exitIO)
		}
		if severity > rc {
			rc = severity
		}
	}
	os.Exit(rc)
}

func validateOne(path string) (result, int) {
	r := result{Path: path}
	manifestPath, err := resolveManifestPath(path)
	if err != nil {
		r.Reason = err.Error()
		return r, exitIO
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		r.Reason = "read: " + err.Error()
		return r, exitIO
	}
	m, err := skill.Parse(content)
	if err != nil {
		if errors.Is(err, skill.ErrManifestInvalid) {
			r.Reason = err.Error()
			return r, exitInvalid
		}
		r.Reason = err.Error()
		return r, exitIO
	}
	r.OK = true
	r.Name = m.Name
	return r, exitOK
}

func resolveManifestPath(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return filepath.Join(path, "SKILL.md"), nil
	}
	return path, nil
}
