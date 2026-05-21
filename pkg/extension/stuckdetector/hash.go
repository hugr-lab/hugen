package stuckdetector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// LocalToolHash mirrors `pkg/models/hugr.go::hashToolCall` in
// shape so providers that don't fill `model.ChunkToolCall.Hash`
// still produce a hash equal to what the Hugr provider would
// produce for the same args. Inputs: canonical tool name +
// JSON-marshaled args. Output: hex-encoded sha-256.
//
// Phase 5.2.η.4 — moved from `pkg/session/turn_stuck.go::sessionToolHash`.
// Exposed (capital L) so callers outside the detector that need
// the same hash (none today; reserved for future cross-extension
// observability) can reach it.
func LocalToolHash(name string, args any) string {
	raw, _ := json.Marshal(args)
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

// toolErrorResponse mirrors the JSON shape tool providers stuff
// into `protocol.ToolResultPayload.Result` on error. The
// `error.code` discriminator drives the repeated_error detector.
type toolErrorResponse struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// parseToolErrorCode extracts the `error.code` string from a
// tool_result payload when the frame's IsError flag is set.
// Returns "" on success, "unknown" on a malformed error envelope
// — the repeated_error detector needs *some* signal to cluster
// by, even when the provider's payload doesn't speak the
// expected shape.
func parseToolErrorCode(payload []byte) string {
	if len(payload) == 0 {
		return "unknown"
	}
	var env toolErrorResponse
	if err := json.Unmarshal(payload, &env); err != nil {
		return "unknown"
	}
	if env.Error.Code == "" {
		return "unknown"
	}
	return env.Error.Code
}
