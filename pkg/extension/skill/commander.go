package skill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// Phase-4.1b-pre stage 4a: Commander migration.
//
// `/skill list | load <name> | unload <name>` moves off
// pkg/runtime onto the skill extension via the generic
// [extension.Commander] capability so the slash and tool paths
// share one home. Operator-tier permission gating
// (hugen:skill:load:<name>) stays unchanged; the slash path keeps
// emitting SystemMarker diagnostics, plus — new in stage 3+4 —
// the same ExtensionFrame the tool path emits, so Recovery sees
// slash-driven loads after a process restart.

// permObjectSkill is the Tier-1 permission section for skill
// operations. Fields are "load:<name>", "unload:<name>" and "*";
// see specs/003-agent-runtime-phase-3 §7.1.
const permObjectSkill = "hugen:skill"

// Compile-time assertion.
var _ extension.Commander = (*Extension)(nil)

// Commands implements [extension.Commander]. Returns the single
// /skill entry; the handler dispatches the three subcommands
// (list/load/unload) from one closure.
func (e *Extension) Commands() []extension.Command {
	return []extension.Command{
		{
			Name:        "skill",
			Description: "list, load, unload or uninstall skills: /skill list | /skill load <name> | /skill unload <name> | /skill uninstall <name>",
			Handler:     e.handleSkillCommand,
		},
	}
}

func (e *Extension) handleSkillCommand(ctx context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	if e.manager == nil {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "skill_unavailable",
				"skill subsystem not configured", true),
		}, nil
	}
	if len(args) == 0 {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "usage_error",
				"usage: /skill list | /skill load <name> | /skill unload <name>", false),
		}, nil
	}
	ctx = perm.WithSession(ctx, perm.SessionContext{SessionID: state.SessionID()})
	switch args[0] {
	case "list":
		return e.handleSkillList(ctx, state, env)
	case "load":
		return e.handleSkillLoad(ctx, state, env, args[1:])
	case "unload":
		return e.handleSkillUnload(ctx, state, env, args[1:])
	case "uninstall":
		return e.handleSkillUninstall(ctx, state, env, args[1:])
	default:
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "usage_error",
				fmt.Sprintf("unknown subcommand %q; try /skill list|load|unload|uninstall", args[0]), false),
		}, nil
	}
}

func (e *Extension) handleSkillList(ctx context.Context, state extension.SessionState, env extension.CommandContext) ([]protocol.Frame, error) {
	available, err := e.manager.List(ctx)
	if err != nil {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "skill_list_failed",
				fmt.Sprintf("list skills: %v", err), true),
		}, nil
	}

	loaded := map[string]bool{}
	if h := FromState(state); h != nil {
		for _, n := range h.LoadedNames(ctx) {
			loaded[n] = true
		}
	}

	type entry struct {
		name   string
		origin string
	}
	groups := make(map[string][]entry)
	for _, s := range available {
		o := s.Origin.String()
		groups[o] = append(groups[o], entry{name: s.Manifest.Name, origin: o})
	}
	originOrder := []string{"system", "dynamic", "local", "community", "inline", "hub"}

	var b strings.Builder
	b.WriteString("Available skills:\n")
	for _, o := range originOrder {
		es := groups[o]
		if len(es) == 0 {
			continue
		}
		sort.Slice(es, func(i, j int) bool { return es[i].name < es[j].name })
		fmt.Fprintf(&b, "  [%s]\n", o)
		for _, en := range es {
			marker := " "
			if loaded[en.name] {
				marker = "*"
			}
			fmt.Fprintf(&b, "    %s %s\n", marker, en.name)
		}
	}
	if len(available) == 0 {
		b.WriteString("  (none)\n")
	}
	b.WriteString("\n* = loaded in this session.")
	return []protocol.Frame{
		protocol.NewAgentMessage(state.SessionID(), env.AgentAuthor, b.String(), 0, true),
	}, nil
}

func (e *Extension) handleSkillLoad(ctx context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	if len(args) == 0 || args[0] == "" {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "usage_error",
				"usage: /skill load <name>", false),
		}, nil
	}
	name := args[0]

	if e.perms != nil {
		p, err := e.perms.Resolve(ctx, permObjectSkill, "load:"+name)
		if err != nil {
			return []protocol.Frame{
				protocol.NewError(state.SessionID(), env.AgentAuthor, "permission_resolve_failed",
					err.Error(), true),
			}, nil
		}
		if p.Disabled {
			return []protocol.Frame{
				protocol.NewToolResult(
					state.SessionID(), env.AgentAuthor, "skill_load:"+name,
					protocol.ToolError{
						Code:    protocol.ToolErrorPermissionDenied,
						Message: fmt.Sprintf("loading skill %q is disabled by operator policy", name),
						Tier:    "config",
					},
					true,
				),
				protocol.NewSystemMarker(state.SessionID(), env.AgentAuthor, protocol.SubjectToolDenied,
					map[string]any{"action": "skill_load", "skill": name, "tier": "config"}),
			}, nil
		}
	}

	h := FromState(state)
	if h == nil {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "skill_unavailable",
				"skill extension not initialised", true),
		}, nil
	}
	if err := h.Load(ctx, name); err != nil {
		code := "skill_load_failed"
		switch {
		case errors.Is(err, skillpkg.ErrSkillNotFound):
			code = "skill_not_found"
		case errors.Is(err, skillpkg.ErrSkillCycle):
			code = "skill_dependency_cycle"
		case errors.Is(err, skillpkg.ErrUnresolvedToolGrant):
			code = "skill_unresolved_tool_grant"
		}
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, code, err.Error(), true),
		}, nil
	}

	// Persistence — same ExtensionFrame the tool path emits, so
	// Recovery sees slash-driven loads after restart.
	if frame, err := newLoadFrame(state.SessionID(), env.AgentAuthor, name); err == nil {
		if eerr := state.Emit(ctx, frame); eerr != nil {
			slog.Warn("skill: persist slash skill-load frame failed", "session", state.SessionID(), "err", eerr)
		}
	}

	return []protocol.Frame{
		protocol.NewSystemMarker(state.SessionID(), env.AgentAuthor, protocol.SubjectSkillLoaded,
			map[string]any{"skill": name}),
	}, nil
}

func (e *Extension) handleSkillUnload(ctx context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	if len(args) == 0 || args[0] == "" {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "usage_error",
				"usage: /skill unload <name>", false),
		}, nil
	}
	name := args[0]
	h := FromState(state)
	if h == nil {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "skill_unavailable",
				"skill extension not initialised", true),
		}, nil
	}
	if err := h.Unload(ctx, name); err != nil {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "skill_unload_failed",
				err.Error(), true),
		}, nil
	}
	if frame, err := newUnloadFrame(state.SessionID(), env.AgentAuthor, name); err == nil {
		if eerr := state.Emit(ctx, frame); eerr != nil {
			slog.Warn("skill: persist slash skill-unload frame failed", "session", state.SessionID(), "err", eerr)
		}
	}
	return []protocol.Frame{
		protocol.NewSystemMarker(state.SessionID(), env.AgentAuthor, protocol.SubjectSkillUnloaded,
			map[string]any{"skill": name}),
	}, nil
}

// handleSkillUninstall removes a dynamic skill from the store entirely
// — both the on-disk bundle and its DB index row (Phase 6.2.db). This
// is the explicit operator removal path; unlike /skill unload (which
// only drops it from the current session) the skill is gone from the
// catalogue until re-saved / re-installed. No-op surface when no
// dynamic backend is wired (ErrUnsupportedBackend).
func (e *Extension) handleSkillUninstall(ctx context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	if len(args) == 0 || args[0] == "" {
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, "usage_error",
				"usage: /skill uninstall <name>", false),
		}, nil
	}
	name := args[0]
	if err := e.manager.Uninstall(ctx, name); err != nil {
		code := "skill_uninstall_failed"
		if errors.Is(err, skillpkg.ErrUnsupportedBackend) {
			code = "skill_uninstall_unsupported"
		}
		return []protocol.Frame{
			protocol.NewError(state.SessionID(), env.AgentAuthor, code, err.Error(), true),
		}, nil
	}
	return []protocol.Frame{
		protocol.NewAgentMessage(state.SessionID(), env.AgentAuthor,
			fmt.Sprintf("uninstalled skill %q (bundle + index removed)", name), 0, true),
	}, nil
}
