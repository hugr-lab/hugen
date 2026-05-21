package stuckdetector

import (
	"sync"
	"time"
)

// detector tuning. Values mirror the legacy constants from
// `pkg/session/turn_stuck.go` (η.3 baseline). All four detectors
// share recentHashes; the cap is max of every window/count below.
const (
	// stuckRepeatedHashWindow is N for the repeated_hash detector
	// — N identical-hash calls in a row trigger the rising edge.
	stuckRepeatedHashWindow = 3

	// stuckTightDensityCount + stuckTightDensityWindow tune the
	// tight_density detector — M same-hash calls within W seconds.
	stuckTightDensityCount  = 3
	stuckTightDensityWindow = 2 * time.Second

	// stuckRepeatedErrorCount / stuckRepeatedErrorWindow tune the
	// repeated_error detector — K errored results sharing the
	// same (tool, code) within a trailing window of W samples.
	stuckRepeatedErrorCount  = 3
	stuckRepeatedErrorWindow = 6
)

// DetectorState owns the runtime side of the four detectors.
// One handle per session, mutated from the session's emit hot
// path (FrameObserver runs on the Run goroutine — see
// `pkg/session/session.go::notifyFrameObservers`). Restart
// resets every field to zero by design (spec: in-memory only).
type DetectorState struct {
	mu sync.Mutex

	// recentHashes is the trailing window of tool-call samples
	// used by every detector. Cap = max of every constant above;
	// we trim by length so the slice never grows past that bound.
	recentHashes []hashSample

	// Rising-edge flags. A nudge fires only on false→true
	// transitions. A break in the pattern (different hash,
	// density relaxed below threshold, no error chain) clears
	// the flag so a later recurrence can fire again.
	repeatedHashActive  bool
	tightDensityActive  bool
	repeatedErrorActive bool
	noProgressActive    bool
}

// hashSample is one entry in the trailing window: the hash of the
// dispatched tool call + bookkeeping the detectors need.
type hashSample struct {
	hash    string
	tool    string    // canonical tool name; lets repeated_error cluster across hashes
	toolID  string    // matches a ToolResult frame's Payload.ToolID
	errCode string    // populated when the matching tool_result arrives; "" = success
	at      time.Time // wall-clock dispatch timestamp (used by tight_density)
}

// recordSample appends one entry, trimming the slice to the
// configured cap. The first newer sample evicts the oldest.
func (s *DetectorState) recordSample(sample hashSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maxLen := stuckRepeatedHashWindow
	if stuckTightDensityCount > maxLen {
		maxLen = stuckTightDensityCount
	}
	if stuckRepeatedErrorWindow > maxLen {
		maxLen = stuckRepeatedErrorWindow
	}
	s.recentHashes = append(s.recentHashes, sample)
	if extra := len(s.recentHashes) - maxLen; extra > 0 {
		s.recentHashes = append(s.recentHashes[:0], s.recentHashes[extra:]...)
	}
}

// annotateResult finds the sample whose toolID matches and sets
// its errCode. Returns ok=true when a match was found. The most
// common case is "annotate the last entry" so the function walks
// the slice in reverse.
func (s *DetectorState) annotateResult(toolID, errCode string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.recentHashes) - 1; i >= 0; i-- {
		if s.recentHashes[i].toolID == toolID {
			s.recentHashes[i].errCode = errCode
			return true
		}
	}
	return false
}

// snapshot returns a fresh copy of recentHashes. Used by the
// detector evaluation pass so the locked section stays narrow.
func (s *DetectorState) snapshot() []hashSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recentHashes) == 0 {
		return nil
	}
	out := make([]hashSample, len(s.recentHashes))
	copy(out, s.recentHashes)
	return out
}

// setRepeatedHashActive returns whether the call transitioned
// inactive→active. The detector caller uses this to decide
// whether to emit a nudge frame.
func (s *DetectorState) setRepeatedHashActive(v bool) (transitioned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == s.repeatedHashActive {
		return false
	}
	prev := s.repeatedHashActive
	s.repeatedHashActive = v
	return !prev && v
}

func (s *DetectorState) setTightDensityActive(v bool) (transitioned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == s.tightDensityActive {
		return false
	}
	prev := s.tightDensityActive
	s.tightDensityActive = v
	return !prev && v
}

func (s *DetectorState) setRepeatedErrorActive(v bool) (transitioned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == s.repeatedErrorActive {
		return false
	}
	prev := s.repeatedErrorActive
	s.repeatedErrorActive = v
	return !prev && v
}

func (s *DetectorState) setNoProgressActive(v bool) (transitioned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == s.noProgressActive {
		return false
	}
	prev := s.noProgressActive
	s.noProgressActive = v
	return !prev && v
}
