// context_provider.go — Stage 2 (L3) synthetic `context:*` tools. A
// standalone [tool.ToolProvider] named "context" (NOT the compactor
// extension itself, whose provider name is "compactor" and would
// reject context-prefixed tool names at AddProvider time). The
// provider is stateless: every Call recovers the calling session's
// [*CompactorState] from the dispatch context (exactly how the
// mission extension's tools recover MissionState), so one provider
// value serves every session.
//
// The four ops drive the checkpoint state in checkpoints.go:
//   - context:checkpoint(description) — close the current segment.
//   - context:hide(cp_id)             — collapse a closed segment.
//   - context:expand(cp_id)           — restore a hidden segment.
//   - context:rollback(cp_id, note)   — destructively drop a segment.
//
// These tools are EXEMPT from the checkpoint_required / context_full
// dispatch blocks (pkg/session) — they are the only calls that pass
// while the model is blocked, so it can always recover.
package compactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// ContextProviderName is the provider discriminator for the synthetic
// context:* tools. Aliased to [extension.ContextProviderName] (defined
// in the interface package so pkg/session can identify context tools
// as block-exempt without importing this concrete package) so the two
// can never drift.
const ContextProviderName = extension.ContextProviderName

// Permission objects gated by the 3-tier perm stack. The context
// tools are universally available where granted (the `_system`
// skill); these objects let an operator policy still scope them.
const (
	PermContextCheckpoint = "hugen:context:checkpoint"
	PermContextHide       = "hugen:context:hide"
	PermContextExpand     = "hugen:context:expand"
	PermContextRollback   = "hugen:context:rollback"
)

const (
	contextCheckpointSchema = `{
  "type": "object",
  "properties": {
    "description": {
      "type": "string",
      "description": "Short label for the work in the segment you are closing (e.g. \"read + parsed devices.json\"). Shown in the checkpoint list and as the placeholder when the segment is hidden."
    }
  },
  "required": ["description"]
}`

	contextHideSchema = `{
  "type": "object",
  "properties": {
    "cp_id": {
      "type": "string",
      "description": "The checkpoint id whose segment to collapse (e.g. \"cp-1\"), from the checkpoint list. Reversible with context:expand."
    }
  },
  "required": ["cp_id"]
}`

	contextExpandSchema = `{
  "type": "object",
  "properties": {
    "cp_id": {
      "type": "string",
      "description": "The hidden checkpoint id to restore to full detail."
    }
  },
  "required": ["cp_id"]
}`

	contextRollbackSchema = `{
  "type": "object",
  "properties": {
    "cp_id": {
      "type": "string",
      "description": "Restore point: every history entry after this checkpoint is DROPPED (not hidden — gone). Use when the work after a checkpoint went wrong."
    },
    "note": {
      "type": "string",
      "description": "One line recording why you rolled back, kept in place of the dropped work."
    }
  },
  "required": ["cp_id"]
}`
)

// ContextProvider is the stateless host for the context:* tools.
type ContextProvider struct{}

// NewContextProvider constructs the provider. It carries no state —
// every Call resolves the session from the dispatch context.
func NewContextProvider() *ContextProvider { return &ContextProvider{} }

// compile-time assertion.
var _ tool.ToolProvider = (*ContextProvider)(nil)

// Name implements [tool.ToolProvider].
func (p *ContextProvider) Name() string { return ContextProviderName }

// Lifetime implements [tool.ToolProvider]. Stateless / agent-lived.
func (p *ContextProvider) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// Subscribe implements [tool.ToolProvider]. Static catalogue.
func (p *ContextProvider) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements [tool.ToolProvider]. Nothing to release.
func (p *ContextProvider) Close() error { return nil }

// List implements [tool.ToolProvider].
func (p *ContextProvider) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             ContextProviderName + ":checkpoint",
			Description:      "Close the current context segment with a short description so its tokens become hideable later. Call it when a chunk of work (a big read, a parse, a sub-investigation) is finished — or when the runtime tells you the segment window is exceeded.",
			Provider:         ContextProviderName,
			PermissionObject: PermContextCheckpoint,
			ArgSchema:        json.RawMessage(contextCheckpointSchema),
		},
		{
			Name:             ContextProviderName + ":hide",
			Description:      "Collapse a closed checkpoint-segment to a one-line placeholder, shedding its tokens from your context. Reversible with context:expand. Use it when context is filling and an earlier segment's detail is no longer needed.",
			Provider:         ContextProviderName,
			PermissionObject: PermContextHide,
			ArgSchema:        json.RawMessage(contextHideSchema),
		},
		{
			Name:             ContextProviderName + ":expand",
			Description:      "Restore a previously hidden checkpoint-segment to full detail when you need it again.",
			Provider:         ContextProviderName,
			PermissionObject: PermContextExpand,
			ArgSchema:        json.RawMessage(contextExpandSchema),
		},
		{
			Name:             ContextProviderName + ":rollback",
			Description:      "Destructively drop every history entry after a checkpoint (not reversible). Use when the work after a checkpoint went wrong and you want to start that stretch over.",
			Provider:         ContextProviderName,
			PermissionObject: PermContextRollback,
			ArgSchema:        json.RawMessage(contextRollbackSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider]. Routes by short tool name.
func (p *ContextProvider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := strings.TrimPrefix(name, ContextProviderName+":")
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return ctxToolErr("session_gone", "no session attached to dispatch ctx")
	}
	s := FromState(state)
	if s == nil {
		return ctxToolErr("unavailable", "context checkpoints not initialised on this session")
	}
	switch short {
	case "checkpoint":
		return p.callCheckpoint(s, args)
	case "hide":
		return p.callHide(s, args)
	case "expand":
		return p.callExpand(s, args)
	case "rollback":
		return p.callRollback(s, args)
	default:
		return nil, fmt.Errorf("%w: context:%s", tool.ErrUnknownTool, short)
	}
}

type checkpointInput struct {
	Description string `json:"description"`
}

type cpIDInput struct {
	CpID string `json:"cp_id"`
}

type rollbackInput struct {
	CpID string `json:"cp_id"`
	Note string `json:"note"`
}

func (p *ContextProvider) callCheckpoint(s *CompactorState, args json.RawMessage) (json.RawMessage, error) {
	var in checkpointInput
	if err := json.Unmarshal(args, &in); err != nil {
		return ctxToolErr("bad_request", fmt.Sprintf("invalid context:checkpoint args: %v", err))
	}
	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = "(unlabelled segment)"
	}
	cp := s.AddCheckpoint(desc)
	return checkpointListResponse(s, map[string]any{
		"ok":         true,
		"checkpoint": cp.ID,
		"message":    fmt.Sprintf("segment closed as %s; counter reset. Hide closed segments with context:hide(cp_id) when context fills.", cp.ID),
	})
}

func (p *ContextProvider) callHide(s *CompactorState, args json.RawMessage) (json.RawMessage, error) {
	var in cpIDInput
	if err := json.Unmarshal(args, &in); err != nil {
		return ctxToolErr("bad_request", fmt.Sprintf("invalid context:hide args: %v", err))
	}
	id := strings.TrimSpace(in.CpID)
	if id == "" {
		return ctxToolErr("bad_request", "cp_id is required")
	}
	cp, ok := s.SetCheckpointHidden(id, true)
	if !ok {
		return ctxToolErr("not_found", fmt.Sprintf("no checkpoint %q; %s", id, knownCheckpointIDs(s)))
	}
	return checkpointListResponse(s, map[string]any{
		"ok":      true,
		"hidden":  cp.ID,
		"message": fmt.Sprintf("%s collapsed (~%d tokens shed next iteration). context:expand(cp_id=%q) to restore.", cp.ID, cp.Tokens, cp.ID),
	})
}

func (p *ContextProvider) callExpand(s *CompactorState, args json.RawMessage) (json.RawMessage, error) {
	var in cpIDInput
	if err := json.Unmarshal(args, &in); err != nil {
		return ctxToolErr("bad_request", fmt.Sprintf("invalid context:expand args: %v", err))
	}
	id := strings.TrimSpace(in.CpID)
	if id == "" {
		return ctxToolErr("bad_request", "cp_id is required")
	}
	// v1: no estimate-based size guard here (the provider can't see the
	// tier budget / real occupancy). If expand re-enters the 0.80 band,
	// trigger 2 fires again next iteration and the model sheds another
	// segment — a self-correcting loop, one extra round-trip. The
	// precise post-expand guard (§6.5) is a follow-up.
	cp, ok := s.SetCheckpointHidden(id, false)
	if !ok {
		return ctxToolErr("not_found", fmt.Sprintf("no checkpoint %q; %s", id, knownCheckpointIDs(s)))
	}
	return checkpointListResponse(s, map[string]any{
		"ok":       true,
		"expanded": cp.ID,
		"message":  fmt.Sprintf("%s restored to full detail.", cp.ID),
	})
}

func (p *ContextProvider) callRollback(s *CompactorState, args json.RawMessage) (json.RawMessage, error) {
	var in rollbackInput
	if err := json.Unmarshal(args, &in); err != nil {
		return ctxToolErr("bad_request", fmt.Sprintf("invalid context:rollback args: %v", err))
	}
	id := strings.TrimSpace(in.CpID)
	if id == "" {
		return ctxToolErr("bad_request", "cp_id is required")
	}
	dropped, ok := s.rollbackFrom(id)
	if !ok {
		return ctxToolErr("not_found", fmt.Sprintf("no checkpoint %q; %s", id, knownCheckpointIDs(s)))
	}
	note := strings.TrimSpace(in.Note)
	msg := fmt.Sprintf("rolled back to %s; %d entries dropped.", id, dropped)
	if note != "" {
		msg += " note: " + note
	}
	return checkpointListResponse(s, map[string]any{
		"ok":              true,
		"rolled_back_to":  id,
		"entries_dropped": dropped,
		"message":         msg,
	})
}

// checkpointListResponse merges op-specific fields with the current
// checkpoint list + segment/hidden token totals so every context:*
// result hands the model an up-to-date menu of what it can hide /
// expand / roll back.
func checkpointListResponse(s *CompactorState, extra map[string]any) (json.RawMessage, error) {
	cps := s.Checkpoints()
	out := make(map[string]any, len(extra)+3)
	for k, v := range extra {
		out[k] = v
	}
	out["checkpoints"] = cps
	out["current_segment_tokens"] = s.SegmentTokens()
	out["hidden_tokens"] = s.HiddenSegmentTokens()
	return json.Marshal(out)
}

// knownCheckpointIDs renders the available ids for a not_found error so
// a weak model corrects a wrong id instead of guessing.
func knownCheckpointIDs(s *CompactorState) string {
	cps := s.Checkpoints()
	if len(cps) == 0 {
		return "no checkpoints have been created yet"
	}
	ids := make([]string, 0, len(cps))
	for _, cp := range cps {
		ids = append(ids, cp.ID)
	}
	return "known checkpoints: " + strings.Join(ids, ", ")
}

// ctxToolErr mirrors the mission package's structured tool-error
// envelope so the model reads a stable { error: { code, message } }
// shape across synthetic providers.
func ctxToolErr(code, msg string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}
