package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// permObjectSkill is the Tier-1 permission section for skill
// operations. Fields are "load:<name>", "publish:<name>" and "*";
// see specs/003-agent-runtime-phase-3 §7.1.
const permObjectSkill = "hugen:skill"

// skillCommandHandler dispatches the /skill subcommands. Built as
// a closure over the SkillManager + perm.Service so the runtime
// CommandRegistry stays thin.
//
// Subcommands:
//   - /skill list                — show available + loaded skills
//   - /skill load <name>         — load <name> into the session
//   - /skill unload <name>       — unload <name>
func skillCommandHandler(skills *skill.SkillManager, store skill.SkillStore, perms perm.Service) session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		if len(args) == 0 {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "usage_error",
					"usage: /skill list | /skill load <name> | /skill unload <name>", false),
			}, nil
		}
		ctx = perm.WithSession(ctx, perm.SessionContext{SessionID: env.Session.ID()})
		switch args[0] {
		case "list":
			return skillListHandler(ctx, env, skills, store)
		case "load":
			return skillLoadHandler(ctx, env, args[1:], skills, perms)
		case "unload":
			return skillUnloadHandler(ctx, env, args[1:], skills)
		default:
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "usage_error",
					fmt.Sprintf("unknown subcommand %q; try /skill list|load|unload", args[0]), false),
			}, nil
		}
	}
}

func skillListHandler(ctx context.Context, env session.CommandEnv, skills *skill.SkillManager, store skill.SkillStore) ([]protocol.Frame, error) {
	available, err := store.List(ctx)
	if err != nil {
		return []protocol.Frame{
			protocol.NewError(env.Session.ID(), env.AgentAuthor, "skill_list_failed",
				fmt.Sprintf("list skills: %v", err), true),
		}, nil
	}
	loaded := loadedSkillNames(ctx, skills, env.Session.ID())

	type entry struct {
		name   string
		origin string
	}
	groups := make(map[string][]entry)
	for _, s := range available {
		o := s.Origin.String()
		groups[o] = append(groups[o], entry{name: s.Manifest.Name, origin: o})
	}
	originOrder := []string{"system", "local", "community", "inline", "hub"}

	var b strings.Builder
	b.WriteString("Available skills:\n")
	for _, o := range originOrder {
		es := groups[o]
		if len(es) == 0 {
			continue
		}
		sort.Slice(es, func(i, j int) bool { return es[i].name < es[j].name })
		fmt.Fprintf(&b, "  [%s]\n", o)
		for _, e := range es {
			marker := " "
			if loaded[e.name] {
				marker = "*"
			}
			fmt.Fprintf(&b, "    %s %s\n", marker, e.name)
		}
	}
	if len(available) == 0 {
		b.WriteString("  (none)\n")
	}
	b.WriteString("\n* = loaded in this session.")
	return []protocol.Frame{
		protocol.NewAgentMessage(env.Session.ID(), env.AgentAuthor, b.String(), 0, true),
	}, nil
}

func skillLoadHandler(ctx context.Context, env session.CommandEnv, args []string, skills *skill.SkillManager, perms perm.Service) ([]protocol.Frame, error) {
	if len(args) == 0 || args[0] == "" {
		return []protocol.Frame{
			protocol.NewError(env.Session.ID(), env.AgentAuthor, "usage_error",
				"usage: /skill load <name>", false),
		}, nil
	}
	name := args[0]

	p, err := perms.Resolve(ctx, permObjectSkill, "load:"+name)
	if err != nil {
		return []protocol.Frame{
			protocol.NewError(env.Session.ID(), env.AgentAuthor, "permission_resolve_failed",
				err.Error(), true),
		}, nil
	}
	if p.Disabled {
		return []protocol.Frame{
			protocol.NewToolResult(
				env.Session.ID(), env.AgentAuthor, "skill_load:"+name,
				protocol.ToolError{
					Code:    protocol.ToolErrorPermissionDenied,
					Message: fmt.Sprintf("loading skill %q is disabled by operator policy", name),
					Tier:    "config",
				},
				true,
			),
			protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, protocol.SubjectToolDenied,
				map[string]any{"action": "skill_load", "skill": name, "tier": "config"}),
		}, nil
	}

	if err := skills.Load(ctx, env.Session.ID(), name); err != nil {
		code := "skill_load_failed"
		switch {
		case errors.Is(err, skill.ErrSkillNotFound):
			code = "skill_not_found"
		case errors.Is(err, skill.ErrSkillCycle):
			code = "skill_dependency_cycle"
		case errors.Is(err, skill.ErrUnresolvedToolGrant):
			code = "skill_unresolved_tool_grant"
		}
		return []protocol.Frame{
			protocol.NewError(env.Session.ID(), env.AgentAuthor, code, err.Error(), true),
		}, nil
	}
	return []protocol.Frame{
		protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, protocol.SubjectSkillLoaded,
			map[string]any{"skill": name}),
	}, nil
}

func skillUnloadHandler(ctx context.Context, env session.CommandEnv, args []string, skills *skill.SkillManager) ([]protocol.Frame, error) {
	if len(args) == 0 || args[0] == "" {
		return []protocol.Frame{
			protocol.NewError(env.Session.ID(), env.AgentAuthor, "usage_error",
				"usage: /skill unload <name>", false),
		}, nil
	}
	name := args[0]
	if err := skills.Unload(ctx, env.Session.ID(), name); err != nil {
		return []protocol.Frame{
			protocol.NewError(env.Session.ID(), env.AgentAuthor, "skill_unload_failed",
				err.Error(), true),
		}, nil
	}
	return []protocol.Frame{
		protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, protocol.SubjectSkillUnloaded,
			map[string]any{"skill": name}),
	}, nil
}

func loadedSkillNames(ctx context.Context, skills *skill.SkillManager, sessionID string) map[string]bool {
	out := map[string]bool{}
	if skills == nil {
		return out
	}
	for _, n := range skills.LoadedNames(ctx, sessionID) {
		out[n] = true
	}
	return out
}
