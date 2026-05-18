package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveOutDir validates the LLM-supplied output directory and
// returns its absolute path under the calling session's workspace
// directory, mkdir'd and ready to receive part files.
//
// `sessionDir` is the absolute path the runtime provides via MCP
// `_meta.session_dir` (resolved by the workspace extension — under
// the 5.4 layout this is the mission's shared dir for worker
// sessions, so output written by one worker is readable by its
// siblings in the same mission).
//
// Contract:
//
//   - empty `requested`   → default to `<sessionDir>/data/<queryID>/`.
//   - relative path       → joined under `<sessionDir>/`.
//   - absolute path       → rejected (arg_validation).
//   - `..`-escape         → rejected (arg_validation).
func resolveOutDir(sessionDir, requested, queryID string) (string, error) {
	if sessionDir == "" {
		return "", errors.New("session_dir missing in tool call metadata")
	}
	var dir string
	if requested == "" {
		dir = filepath.Join(sessionDir, "data", queryID)
	} else {
		if filepath.IsAbs(requested) {
			return "", &toolError{Code: "arg_validation", Msg: "path must be relative to the session workspace"}
		}
		cleaned := filepath.Clean(requested)
		// filepath.Clean keeps a leading "../" if the path tries
		// to climb above its anchor — that's our escape detector.
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return "", &toolError{Code: "arg_validation", Msg: "path escapes session workspace"}
		}
		dir = filepath.Join(sessionDir, cleaned)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// newShortID returns 8 random hex characters (32 bits of entropy
// — enough to avoid collisions inside one session's data dir).
// crypto/rand failure is treated as fatal: a deterministic id
// would silently shadow another query's output.
func newShortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("crypto/rand: %w", err))
	}
	return hex.EncodeToString(b[:])
}
