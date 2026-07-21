package runtime

import (
	"context"
	"fmt"
	"maps"
	"sort"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Console-managed tool providers: the remote HTTP/SSE MCP servers a hub admin /
// agent owner adds or edits. They are always per_agent — registered on the ROOT
// ToolManager — so an update becomes visible to every live session at once (the
// root generation bump folds into each session's child, see ToolManager.ToolGen).
// stdio providers (bash / hugr-query / python) and per_session providers are out
// of scope: they stay config/skill-driven and are never touched here.
//
// Flow: the hub persists the desired set into the agent's config_override, then
// POSTs /v1/tool-providers/reload; ReloadToolProviders re-reads the authoritative
// (merged) config and reconciles the root — add new, remove dropped, replace
// changed — against managedToolProviders (seeded at boot). AddBySpec validates by
// connecting, so an unreachable endpoint surfaces as a failure, not a live
// registration.

// ToolProviderInfo is one row of the tool-providers list.
type ToolProviderInfo struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Endpoint  string `json:"endpoint,omitempty"`
	Auth      string `json:"auth,omitempty"`
	// Live reports whether a provider by this name is currently on the root.
	Live bool `json:"live"`
}

// ToolProviderReloadResult reports one reconcile pass.
type ToolProviderReloadResult struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Failed  []string `json:"failed,omitempty"`
}

// isManagedProvider reports whether a config spec is console-managed: a per_agent
// MCP over an HTTP/SSE transport.
func isManagedProvider(p config.ToolProviderSpec) bool {
	if p.Type != "" && p.Type != "mcp" {
		return false
	}
	return tool.EffectiveLifetime(p) == tool.LifetimePerAgent && tool.IsHTTPTransport(p.Transport)
}

// seedManagedToolProviders records the boot-time managed set (called from
// phaseTools once Init has loaded the config providers onto the root, so the
// type's own HTTP providers count as already-applied).
func (c *Core) seedManagedToolProviders() {
	c.toolProvidersMu.Lock()
	defer c.toolProvidersMu.Unlock()
	c.managedToolProviders = managedFrom(c.Config.ToolProviders().Providers())
}

func managedFrom(specs []config.ToolProviderSpec) map[string]config.ToolProviderSpec {
	out := map[string]config.ToolProviderSpec{}
	for _, p := range specs {
		if isManagedProvider(p) {
			out[p.Name] = p
		}
	}
	return out
}

// desiredManaged re-fetches the live agent config and returns the fresh managed
// set. With no live identity (tests / local) it falls back to the boot view.
func (c *Core) desiredManaged(ctx context.Context) (map[string]config.ToolProviderSpec, error) {
	if c.Identity == nil {
		return managedFrom(c.Config.ToolProviders().Providers()), nil
	}
	agent, err := c.Identity.Agent(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent_info re-fetch: %w", err)
	}
	in, err := config.LoadStaticInput(agent.Config, c.Cfg.Mode == "local")
	if err != nil {
		return nil, fmt.Errorf("agent config parse: %w", err)
	}
	return managedFrom(in.ToolProviders), nil
}

// ListToolProviders returns the managed remote-HTTP MCP providers from the live
// agent config, each flagged live if currently on the root ToolManager.
func (c *Core) ListToolProviders(ctx context.Context) ([]ToolProviderInfo, error) {
	desired, err := c.desiredManaged(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	if c.Tools != nil {
		for _, n := range c.Tools.Providers() {
			live[n] = true
		}
	}
	out := make([]ToolProviderInfo, 0, len(desired))
	for _, p := range desired {
		out = append(out, ToolProviderInfo{
			Name: p.Name, Transport: p.Transport, Endpoint: p.Endpoint, Auth: p.Auth,
			Live: live[p.Name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReloadToolProviders reconciles the root ToolManager's managed providers to the
// live agent config: add new, remove dropped, replace changed (by spec). Only
// console-managed (per_agent HTTP/SSE MCP) providers are touched.
func (c *Core) ReloadToolProviders(ctx context.Context) (ToolProviderReloadResult, error) {
	// Serialize whole passes: two concurrent reloads could both try to add the
	// same provider (the second failing "already registered") and desync the
	// tracking map. toolReloadMu is dedicated (not toolProvidersMu, which stays
	// short so ListToolProviders never blocks on a reconcile's network I/O).
	c.toolReloadMu.Lock()
	defer c.toolReloadMu.Unlock()

	desired, err := c.desiredManaged(ctx)
	if err != nil {
		return ToolProviderReloadResult{}, err
	}
	if c.Tools == nil {
		return ToolProviderReloadResult{}, nil
	}

	c.toolProvidersMu.Lock()
	applied := c.managedToolProviders
	c.toolProvidersMu.Unlock()
	if applied == nil {
		applied = map[string]config.ToolProviderSpec{}
	}
	// What is actually on the root right now — a provider can be tracked but not
	// live (its connect failed at boot / a prior reload), so "unchanged" alone
	// isn't enough to skip; it must also be live.
	live := map[string]bool{}
	for _, n := range c.Tools.Providers() {
		live[n] = true
	}

	var res ToolProviderReloadResult
	next := map[string]config.ToolProviderSpec{}

	for name, spec := range desired {
		prev, existed := applied[name]
		unchanged := existed && specsEqual(prev, spec)
		if unchanged && live[name] {
			next[name] = spec // unchanged AND live — nothing to do
			continue
		}
		// (re)apply: the spec changed, OR it isn't live yet (boot-failed / retry).
		removedOld := false
		if live[name] {
			_ = c.Tools.RemoveProvider(ctx, name)
			removedOld = true
		}
		if err := c.Tools.AddBySpec(ctx, tool.SpecFromConfig(spec)); err != nil {
			c.Logger.Warn("tool-provider reload: add failed", "name", name, "err", err)
			res.Failed = append(res.Failed, name)
			// If we removed a live provider to apply a CHANGED spec and the new one
			// failed to connect, restore the old so the tool isn't black-holed.
			if removedOld && existed && !unchanged {
				if rerr := c.Tools.AddBySpec(ctx, tool.SpecFromConfig(prev)); rerr == nil {
					next[name] = prev // still live on the OLD spec; retried next reload
					continue
				}
			}
			next[name] = spec // tracked as desired-but-not-live → retried next reload
			continue
		}
		next[name] = spec
		res.Added = append(res.Added, name)
	}
	// remove dropped (only those actually live need a RemoveProvider call)
	for name := range applied {
		if _, ok := desired[name]; ok {
			continue
		}
		if !live[name] {
			continue // tracked-but-not-live and no longer desired → just drop it
		}
		if err := c.Tools.RemoveProvider(ctx, name); err != nil {
			c.Logger.Warn("tool-provider reload: remove failed", "name", name, "err", err)
		} else {
			res.Removed = append(res.Removed, name)
		}
	}

	c.toolProvidersMu.Lock()
	c.managedToolProviders = next
	c.toolProvidersMu.Unlock()
	return res, nil
}

// specsEqual compares the connection-relevant fields of two provider specs.
func specsEqual(a, b config.ToolProviderSpec) bool {
	return a.Transport == b.Transport && a.Endpoint == b.Endpoint &&
		a.Auth == b.Auth && maps.Equal(a.Headers, b.Headers)
}
