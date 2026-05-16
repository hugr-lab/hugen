package session

import (
	"fmt"
	"strings"
	"unicode"
)

// Subagent naming primitive — Phase 5.2 α.
//
// Every spawn carries a model-supplied short name that becomes the
// addressing identifier the parent uses when calling
// `notify_subagent` / `subagent_cancel` / acceptance tools. Session
// ids stay as the canonical internal id but are de-emphasised on the
// user-visible surface. See
// `design/002-runtime-canonical/phase-5.2-multi-mode-acceptance.md`
// §Subagent naming.

const (
	// SubagentNameMin / Max bound the sanitised name length. Min=2
	// ensures the name has enough body for a reader to recognise it
	// (single-char names get padded with a numeric suffix). Max=32
	// keeps TUI tabs / acceptance-modal rows compact.
	SubagentNameMin = 2
	SubagentNameMax = 32

	// nameFallback is used when SanitizeName receives input that
	// produces an empty result after sanitisation (e.g. all
	// invalid chars stripped). Combined with the collision suffix
	// the runtime still produces a unique identifier.
	nameFallback = "subagent"
)

// SanitizeName produces a kebab-case identifier in [a-z0-9-]{2,32}
// from arbitrary model input. The transformation is total — every
// input maps to a valid name. Steps:
//
//  1. Lowercase.
//  2. Replace any char outside [a-z0-9-] with '-'.
//  3. Collapse consecutive '-'.
//  4. Trim leading / trailing '-'.
//  5. Fall back to "subagent" if the result is empty.
//  6. Truncate to 32 chars (re-trimming trailing '-' after the cut).
//  7. Pad to 2 chars with "-x" if the result is a single rune.
//
// Pure function; safe to call without locks.
func SanitizeName(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || unicode.IsSpace(r) || r == '_':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	out := collapseDashes(b.String())
	out = strings.Trim(out, "-")
	if out == "" {
		out = nameFallback
	}
	if len(out) > SubagentNameMax {
		out = strings.TrimRight(out[:SubagentNameMax], "-")
		if out == "" {
			out = nameFallback
		}
	}
	if len(out) < SubagentNameMin {
		out = out + "-x"
	}
	return out
}

// collapseDashes replaces runs of '-' with a single '-'.
func collapseDashes(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	prevDash := false
	for _, r := range in {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// resolveChildNameLocked returns a unique sanitised name for a new
// child of s. Sanitises raw, then probes for collisions against
// existing live children (`s.children`) and any in-flight spawn
// reservations (`s.pendingNames`). On collision appends `-2`, `-3`,
// … until a free slot is found.
//
// Caller MUST hold s.childMu. The returned name is not yet
// reserved — caller is responsible for adding it to s.pendingNames
// or s.children[child.id]=child (which carries child.name) before
// releasing the lock.
func (s *Session) resolveChildNameLocked(raw string) string {
	base := SanitizeName(raw)
	if !s.nameInUseLocked(base) {
		return base
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("-%d", i)
		candidate := base + suffix
		if len(candidate) > SubagentNameMax {
			candidate = base[:SubagentNameMax-len(suffix)] + suffix
		}
		if !s.nameInUseLocked(candidate) {
			return candidate
		}
	}
}

// nameInUseLocked reports whether name is taken by a live child or
// an in-flight spawn. Caller MUST hold s.childMu.
func (s *Session) nameInUseLocked(name string) bool {
	for _, c := range s.children {
		if c != nil && c.name == name {
			return true
		}
	}
	if _, ok := s.pendingNames[name]; ok {
		return true
	}
	return false
}

// childByName returns the live child of s whose Name matches the
// argument, if any. Acquires s.childMu. Empty name returns (nil,
// false) — names are never empty post-sanitisation, so the only
// legitimate empty input is "lookup not by name", in which case
// callers fall through to direct session_id resolution.
func (s *Session) childByName(name string) (*Session, bool) {
	if name == "" {
		return nil, false
	}
	s.childMu.Lock()
	defer s.childMu.Unlock()
	for _, c := range s.children {
		if c != nil && c.name == name {
			return c, true
		}
	}
	return nil, false
}
