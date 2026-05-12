package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// turn_stuck.go implements the three rising-edge stuck detectors from
// phase-4-spec §8.3. Each detector keeps a per-session "active" flag —
// the nudge fires only on inactive→active transition so the model gets
// one reflective ping per slip into the pattern, not a stream.

// stuckRepeatedHashWindow is the default N for the repeated_hash
// detector (§8.3 table). Three identical-hash calls in a row trigger
// the rising edge.
const stuckRepeatedHashWindow = 3

// stuckTightDensityCount + stuckTightDensityWindow tune the
// tight_density detector — M same-hash calls within W seconds.
const (
	stuckTightDensityCount  = 3
	stuckTightDensityWindow = 2 * time.Second
)

// stuckState owns the runtime side of the three detectors. Singleton
// per Session, mutated only on the Run goroutine — no locks. Restart
// resets every field to zero by design (spec: in-memory only).
type stuckState struct {
	// recentHashes is the trailing window of tool-call hashes used to
	// drive both repeated_hash (any N consecutive equal) and tight_
	// density (any N within window). Cap = max(stuckRepeatedHashWindow,
	// stuckTightDensityCount); we trim by length so the slice never
	// grows past that bound.
	recentHashes []hashSample

	// recentErroredHashes pairs each call with whether its tool_result
	// surfaced as an error. Sampled at handleToolResult time. Used by
	// the no_progress detector — same hash repeated AFTER an errored
	// result is the signature of "model didn't read the error".
	recentErrored []bool

	// Rising-edge flags. nudge fires only on false→true transitions.
	// A break in the pattern (different hash, density relaxed below
	// threshold, no error chain) clears the flag so a later recurrence
	// can fire again.
	repeatedHashActive bool
	tightDensityActive bool
	noProgressActive   bool
}

// hashSample is one entry in the trailing window: the hash of the
// dispatched tool call + the wall-clock timestamp of dispatch.
type hashSample struct {
	hash string
	at   time.Time
}

// stuckObserveCall is invoked from the tool dispatch path right after
// emitting tool_call. Records the call's hash + timestamp in the
// trailing window so the next handleToolResult can decide whether
// the iteration triggers a rising-edge nudge. errored is filled in by
// stuckObserveResult once the tool_result lands.
//
// Hash is sourced from model.ChunkToolCall.Hash when the model
// provider populates it (pkg/models/hugr.go::hashToolCall) and falls
// back to a deterministic local hash so providers that don't compute
// it still get coverage. The local hash mirrors the upstream shape so
// equality semantics match across providers.
func (s *Session) stuckObserveCall(name string, args any, providedHash string, at time.Time) {
	if !s.stuckBuffersEnabled() {
		return
	}
	h := providedHash
	if h == "" {
		h = sessionToolHash(name, args)
	}
	max := stuckRepeatedHashWindow
	if stuckTightDensityCount > max {
		max = stuckTightDensityCount
	}
	s.stuck.recentHashes = append(s.stuck.recentHashes, hashSample{hash: h, at: at})
	s.stuck.recentErrored = append(s.stuck.recentErrored, false)
	if extra := len(s.stuck.recentHashes) - max; extra > 0 {
		s.stuck.recentHashes = append(s.stuck.recentHashes[:0], s.stuck.recentHashes[extra:]...)
		s.stuck.recentErrored = append(s.stuck.recentErrored[:0], s.stuck.recentErrored[extra:]...)
	}
}

// stuckObserveResult flips the trailing-window's last "errored?" flag
// when the matching tool_result surfaced an error. Called from the
// dispatchToolCall error branches just before the function returns.
func (s *Session) stuckObserveResult(errored bool) {
	if len(s.stuck.recentErrored) == 0 {
		return
	}
	s.stuck.recentErrored[len(s.stuck.recentErrored)-1] = errored
}

// stuckBuffersEnabled gates buffer growth on the bindings flag — when
// detection is disabled we don't even bother sampling. Callers below
// also re-check via the session-level enabled gate at fire time.
func (s *Session) stuckBuffersEnabled() bool {
	return s != nil
}

// stuckEvaluate runs the three rising-edge checks and emits one
// nudge per detector that just transitioned inactive→active. Called
// from advanceOrFinish AT a turn boundary, after the iteration's
// tool_results have all landed in s.history, and BEFORE the next
// prompt build — so nudges enter s.history before the model rebuilds
// its prompt.
//
// The detectors are independent; multiple flags can fire on the same
// boundary if a single iteration hits two patterns at once (rare but
// not forbidden — e.g. a tight-density burst that also matches the
// repeated-hash window).
func (s *Session) stuckEvaluate(runCtx context.Context) {
	if !s.stuckDetectionEnabled(runCtx) {
		return
	}
	s.evaluateRepeatedHash(runCtx)
	s.evaluateTightDensity(runCtx)
	s.evaluateNoProgress(runCtx)
}

// evaluateRepeatedHash fires once when the last N=stuckRepeatedHashWindow
// hashes are all equal. The flag clears the moment a different hash
// appears in the tail, allowing later recurrence to trigger again
// (per §13.2 #6).
func (s *Session) evaluateRepeatedHash(runCtx context.Context) {
	N := stuckRepeatedHashWindow
	if len(s.stuck.recentHashes) < N {
		s.stuck.repeatedHashActive = false
		return
	}
	tail := s.stuck.recentHashes[len(s.stuck.recentHashes)-N:]
	first := tail[0].hash
	if first == "" {
		s.stuck.repeatedHashActive = false
		return
	}
	for _, e := range tail[1:] {
		if e.hash != first {
			s.stuck.repeatedHashActive = false
			return
		}
	}
	if s.stuck.repeatedHashActive {
		return // pattern continues; already nudged.
	}
	s.stuck.repeatedHashActive = true
	s.injectStuckNudge(runCtx, s.deps.Prompts.MustRender(
		"interrupts/stuck_repeated_tool",
		map[string]any{"N": N},
	))
}

// evaluateTightDensity fires once when M=stuckTightDensityCount
// same-hash calls land within W=stuckTightDensityWindow seconds.
// Different from repeated_hash in that the calls don't need to be
// consecutive — they only need to share a hash and cluster in time.
func (s *Session) evaluateTightDensity(runCtx context.Context) {
	M := stuckTightDensityCount
	if len(s.stuck.recentHashes) < M {
		s.stuck.tightDensityActive = false
		return
	}
	tail := s.stuck.recentHashes[len(s.stuck.recentHashes)-M:]
	first := tail[0].hash
	if first == "" {
		s.stuck.tightDensityActive = false
		return
	}
	for _, e := range tail[1:] {
		if e.hash != first {
			s.stuck.tightDensityActive = false
			return
		}
	}
	span := tail[len(tail)-1].at.Sub(tail[0].at)
	if span > stuckTightDensityWindow {
		s.stuck.tightDensityActive = false
		return
	}
	if s.stuck.tightDensityActive {
		return
	}
	s.stuck.tightDensityActive = true
	s.injectStuckNudge(runCtx, s.deps.Prompts.MustRender(
		"interrupts/stuck_tight_density",
		map[string]any{"M": M, "Window": stuckTightDensityWindow.String()},
	))
}

// evaluateNoProgress fires (as a system_marker, not a system_message —
// per spec §8.3 the no_progress detector surfaces via adapter only)
// when the latest hash matches a prior hash AND the prior tool_result
// was an error. Doesn't break the loop; just lights the runway.
func (s *Session) evaluateNoProgress(runCtx context.Context) {
	if len(s.stuck.recentHashes) < 2 {
		s.stuck.noProgressActive = false
		return
	}
	last := s.stuck.recentHashes[len(s.stuck.recentHashes)-1]
	hit := false
	for i := len(s.stuck.recentHashes) - 2; i >= 0; i-- {
		if s.stuck.recentHashes[i].hash == last.hash && s.stuck.recentErrored[i] {
			hit = true
			break
		}
	}
	if !hit {
		s.stuck.noProgressActive = false
		return
	}
	if s.stuck.noProgressActive {
		return
	}
	s.stuck.noProgressActive = true
	mk := protocol.NewSystemMarker(s.id, s.agent.Participant(),
		protocol.SubjectNoProgress,
		map[string]any{"hash": last.hash})
	if err := s.emit(runCtx, mk); err != nil {
		s.logger.Warn("session: emit no_progress marker",
			"session", s.id, "err", err)
	}
}

// injectStuckNudge emits a system_message{kind:"stuck_nudge"} Frame
// and folds the rendered line into s.history so the model sees the
// nudge on its next prompt build. Mirrors maybeInjectSoftWarning's
// semantics — local-only, never propagated to the parent (spec §11).
func (s *Session) injectStuckNudge(runCtx context.Context, content string) {
	frame := protocol.NewSystemMessage(s.id, s.agent.Participant(),
		protocol.SystemMessageStuckNudge, content)
	if err := s.emit(runCtx, frame); err != nil {
		s.logger.Warn("session: emit stuck_nudge",
			"session", s.id, "err", err)
		return
	}
	s.history = append(s.history, model.Message{
		Role:    model.RoleUser,
		Content: fmt.Sprintf("[system: %s] %s", protocol.SystemMessageStuckNudge, content),
	})
}

// sessionToolHash mirrors pkg/models/hugr.go::hashToolCall in shape so
// providers that don't fill ChunkToolCall.Hash still produce a hash
// equal to what the Hugr provider would produce for the same args.
// Inputs: canonical tool name + JSON-marshaled args. Output: hex-
// encoded sha-256.
func sessionToolHash(name string, args any) string {
	raw, _ := json.Marshal(args)
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}
