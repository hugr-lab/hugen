# Tool discovery — never invent tool names

`allowed-tools` grants and a task's `allowed_tools_default` must name
EXACT `provider:tool` tools from the live registry. The recurring
authoring failure is inventing a plausible-looking name — usually a
SKILL name treated as a provider (`hugr-data:execute`,
`python-runner:run`). Those are not tools; `skill:save` rejects them.
Look the real names up instead.

## tool:providers()

Lists every registered provider with its tool count:

```
tool:providers()
→ { providers: [
    { name: "bash-mcp",   lifetime: "per_session", tool_count: 6 },
    { name: "python-mcp", lifetime: "per_agent",   tool_count: 3 },
    { name: "hugr-main",  lifetime: "per_agent",   tool_count: 12 },
    ...
  ] }
```

Optional `pattern` substring-filters the provider names.

## tool:tools(provider)

Lists a provider's real tool names:

```
tool:tools(provider: "python-mcp")
→ { tools: [
    { name: "python-mcp:run_script", summary: "Run a Python script ..." },
    { name: "python-mcp:install",    summary: "Install a package ..." },
    ...
  ] }
```

- `detailed: true` adds each tool's argument schema (use when you need
  to know the parameters, not just the name).
- `pattern: "<substr>"` narrows by tool name.

The `name` field is exactly what goes into `allowed_tools_default` /
`allowed-tools`. Copy it verbatim.

## Workflow

1. You know the capability you need ("run a python script", "query
   the catalog") but not the exact tool name.
2. `tool:providers()` → find the provider that offers it.
3. `tool:tools(provider: "<name>")` → copy the exact `provider:tool`.
4. Put that verbatim string in the manifest.

A skill name is NOT a provider. If you find yourself writing a skill
name before the colon, stop — the skill's work runs through a real
provider (python / bash / a hugr provider). Find that provider's tool.
