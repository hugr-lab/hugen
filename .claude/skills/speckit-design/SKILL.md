---
name: "speckit-design"
description: "Research a topic, explore trade-offs, and produce a numbered design document in design/ that feeds into the speckit specify/plan/tasks flow."
argument-hint: "Topic or problem to research and design (e.g. 'intent-based LLM routing')"
compatibility: "Requires spec-kit project structure with .specify/ directory"
metadata:
  author: "hugr-lab"
  source: "templates/design-template.md"
user-invocable: true
disable-model-invocation: true
---

## User Input

```text
$ARGUMENTS
```

You **MUST** consider the user input before proceeding (if not empty).

## Pre-Execution Checks

**Check for extension hooks (before design)**:
- Check if `.specify/extensions.yml` exists in the project root.
- If it exists, read it and look for entries under the `hooks.before_design` key.
- If the YAML cannot be parsed or is invalid, skip hook checking silently and continue normally.
- Filter out hooks where `enabled` is explicitly `false`. Treat hooks without an `enabled` field as enabled by default.
- For each remaining hook, do **not** attempt to interpret or evaluate hook `condition` expressions:
  - If the hook has no `condition` field, or it is null/empty, treat the hook as executable.
  - If the hook defines a non-empty `condition`, skip the hook and leave condition evaluation to the HookExecutor implementation.
- For each executable hook, output the following based on its `optional` flag:
  - **Optional hook** (`optional: true`):
    ```
    ## Extension Hooks

    **Optional Pre-Hook**: {extension}
    Command: `/{command}`
    Description: {description}

    Prompt: {prompt}
    To execute: `/{command}`
    ```
  - **Mandatory hook** (`optional: false`):
    ```
    ## Extension Hooks

    **Automatic Pre-Hook**: {extension}
    Executing: `/{command}`
    EXECUTE_COMMAND: {command}

    Wait for the result of the hook command before proceeding to the Outline.
    ```
- If no hooks are registered or `.specify/extensions.yml` does not exist, skip silently.

## Outline

You are creating a design document for a research and exploration topic. Design documents live in the `design/` directory at the repository root, each in a sequentially numbered subdirectory.

**This is NOT a speckit specification.** The design phase is a pre-speckit research phase. Its output feeds into `/speckit.specify` later.

Follow this execution flow:

### Phase 0: Setup

1. Determine the next design number:
   - Scan the `design/` directory at the repository root for existing numbered subdirectories (format: `NNN-short-name`, e.g. `001-intent-routing`).
   - Find the highest existing number and increment by 1.
   - If no `design/` directory exists, start at `001`.
   - Format: zero-padded to 3 digits (e.g. `001`, `002`, `012`).

2. Generate a short name (2-4 words, lowercase, hyphen-separated) from the user input:
   - Filter stop words, keep meaningful terms.
   - Example: "intent-based LLM routing" -> `intent-llm-routing`.

3. Create the directory: `design/[NNN]-[short-name]/`.

4. Copy the design template from `.specify/templates/design-template.md` into `design/[NNN]-[short-name]/design.md`.
   - If the template does not exist, create the file with basic structure.

### Phase 1: Research

This is the core of the design skill. You MUST perform actual research, not just fill placeholders.

1. **Read the project constitution** at `.specify/memory/constitution.md` to understand principles and constraints that apply.

2. **Read source documents** in `source-doc/` if they exist -- these contain architectural context, ADRs, and platform decisions that inform the design.

3. **Explore the existing codebase** to understand:
   - What already exists that relates to this topic.
   - What interfaces, types, or patterns are established.
   - What the current directory structure looks like.

4. **Research external resources** if needed:
   - Use web search or documentation fetching for libraries, protocols, or APIs.
   - Evaluate dependencies against the constitution's dependency policy.

5. **Identify constraints and trade-offs**:
   - What does the constitution mandate?
   - What does ADK provide vs. what do we need to build?
   - What are the performance, complexity, and maintenance trade-offs?

### Phase 2: Design Document

Fill the design document with research findings. Every section MUST contain concrete content, not placeholders.

**Required sections:**

- **Problem Statement**: What problem we are solving, why it matters, what triggers this work.
- **Research**: Prior art, technical exploration results, constraints discovered.
- **Proposed Design**: Architecture, key interfaces (Go code sketches), data model, dependencies.
- **Trade-offs & Alternatives**: Table of options considered with pros/cons/verdict.
- **Open Questions**: Unresolved items that need clarification before or during spec phase.
- **Spec Input**: A concise summary block that can be copied directly as input to `/speckit.specify`. This should describe the feature in terms of user-visible behavior and requirements, not internal design details.

**Design document quality gates:**

- No remaining template placeholders (bracket tokens like `[FEATURE_NAME]` must be replaced).
- All Go interface sketches must be syntactically plausible.
- Dependencies mentioned must be checked against the constitution's dependency policy.
- At least 2 alternatives in the trade-offs table (even if one is "do nothing").
- Spec Input section must be self-contained and actionable.

### Phase 3: Write

1. Write the completed design document to `design/[NNN]-[short-name]/design.md`.

2. If the research produced supplementary artifacts (interface sketches, data flow diagrams in text, API exploration notes), write them as additional files in the same directory:
   - `design/[NNN]-[short-name]/interfaces.go` -- Go interface sketches (if applicable).
   - `design/[NNN]-[short-name]/notes.md` -- Extended research notes (if the design.md would be too long).

3. Output a summary to the user:
   - Design number and directory path.
   - Key findings from research.
   - Proposed approach (1-2 sentences).
   - Open questions count.
   - Next step: `/speckit.specify <spec input summary>`.

## Post-Execution Checks

**Check for extension hooks (after design)**:
- Check if `.specify/extensions.yml` exists in the project root.
- If it exists, read it and look for entries under the `hooks.after_design` key.
- If the YAML cannot be parsed or is invalid, skip hook checking silently and continue normally.
- Filter out hooks where `enabled` is explicitly `false`. Treat hooks without an `enabled` field as enabled by default.
- For each remaining hook, do **not** attempt to interpret or evaluate hook `condition` expressions:
  - If the hook has no `condition` field, or it is null/empty, treat the hook as executable.
  - If the hook defines a non-empty `condition`, skip the hook and leave condition evaluation to the HookExecutor implementation.
- For each executable hook, output the following based on its `optional` flag:
  - **Optional hook** (`optional: true`):
    ```
    ## Extension Hooks

    **Optional Hook**: {extension}
    Command: `/{command}`
    Description: {description}

    Prompt: {prompt}
    To execute: `/{command}`
    ```
  - **Mandatory hook** (`optional: false`):
    ```
    ## Extension Hooks

    **Automatic Hook**: {extension}
    Executing: `/{command}`
    EXECUTE_COMMAND: {command}
    ```
- If no hooks are registered or `.specify/extensions.yml` does not exist, skip silently.
