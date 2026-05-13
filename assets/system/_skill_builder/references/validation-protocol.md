# Validation protocol — mandatory after every save

A skill that hasn't been validated end-to-end is not done. This
document spells out the validation loop with worked examples.

## Why mandatory

The model writes the manifest, body, and bundled scripts in one
shot. Three classes of error are common:

1. **Manifest-grant typos** — body invokes `python:run_script`
   but `allowed-tools` lists `python-mcp:run_script`. Tool call
   gets denied at dispatch.
2. **Script bugs** — syntax errors, wrong API usage, wrong
   parameter names.
3. **Procedure mismatches** — body says "run X then Y" but X
   doesn't produce what Y expects.

Validation catches all three before the user tries the skill on
real data and finds it broken.

## The loop

After `skill:save` returns success (the skill is auto-loaded
in the current session):

```text
┌─────────────────────────────────────────┐
│ 1. Construct synthetic test parameters  │
│    (NOT the user's actual data)          │
└─────────────────────────────────────────┘
                 ↓
┌─────────────────────────────────────────┐
│ 2. Run the documented procedure         │
│    end-to-end with those parameters     │
└─────────────────────────────────────────┘
                 ↓
              Failed?
              /     \
          Yes/       \No
            ↓         ↓
┌─────────────────┐  ┌─────────────────┐
│ 3a. unload      │  │ 4. Report       │
│ 3b. fix bundle  │  │    "ready" to   │
│ 3c. save with   │  │    user         │
│     overwrite=  │  └─────────────────┘
│     true        │
│ 3d. goto 1      │
└─────────────────┘
```

## Synthetic test parameters

Pick values obviously not the user's data. Conventions:

- Material codes: `TEST-001`, `TEST-002`.
- Date ranges: most-recent-quarter window; if the data source
  doesn't have it, pick the smallest discoverable range.
- Customer / org IDs: well-known synthetic IDs from the data's
  test fixtures, or first-row-by-id.
- Free-text queries: short distinctive strings like
  `validation probe TEST`.

Why synthetic: the user's data may be sensitive; the skill's
correctness should be independent of any specific real value.

## Worked example — material movement report

Saved skill: `material-movement-report`. Body says:

```markdown
1. Run scripts/query_movement.py --material {material_code}
   to fetch movement data → ${WORKSPACE}/movement.parquet
2. Run scripts/render_report.py --data ${WORKSPACE}/movement.parquet
   --template ${SKILL_DIR}/assets/report-template.html.tmpl
   --out ${WORKSPACE}/report.pdf
3. Return the report path to the user.
```

Validation:

1. Pick `material_code=TEST-001`.
2. Read the loaded skill bundle — directory shown in your prompt's
   "Loaded skill bundles" block.
3. Invoke step 1:
   ```
   bash:run python ${dir}/scripts/query_movement.py --material TEST-001
   ```
   - **Expected**: parquet written to workspace, exit 0.
   - **Failure modes**:
     - Script raises ImportError → missing `requirements.txt`
       in bundle, or skill body assumed venv that doesn't exist.
       FIX: bundle the script with stdlib-only fallback OR add
       venv setup step to body.
     - Tool call denied → `allowed-tools` doesn't admit `bash:run`.
       FIX: add it to manifest, re-save with overwrite.
     - SQL/GraphQL error → query template wrong. FIX: fix the
       script, re-save.
4. Invoke step 2 with the test parquet output. Same drill.
5. If both succeed → report ready to user.

## Reporting

After clean validation:

> Saved as `material-movement-report`, tested with
> `material_code=TEST-001` (script ran, report generated).
> Load it in a future session via
> `/skill load material-movement-report`.

Be explicit about what was tested. Don't say "ready" without
having actually run the procedure.

## When validation reveals a manifest fix

The most common fix is `allowed-tools` — script invokes a tool
the manifest didn't list. Flow:

1. `skill:unload material-movement-report`.
2. Compose updated `skill_md` with the missing grant added.
3. `skill:save({skill_md, scripts, ..., overwrite: true})`.
4. Re-run validation from step 1.

`overwrite=true` is authorised here because the user already
opened a save flow; iteration to validate is part of it. Don't
re-ask the user for permission to overwrite during validation.

## When validation reveals a body fix

If the procedure is wrong (steps in wrong order, missing step,
wrong tool choice), edit the body and re-save with overwrite.

If the procedure mostly works but has an edge case (e.g. fails
on materials with no movement records), document the edge case
in the body. Validation isn't trying to handle every edge case
— it's trying to confirm the happy path works.

## When NOT to keep iterating

If after 3-4 iterations the skill still doesn't work:

1. Stop.
2. Tell the user what failed and what you tried.
3. Ask whether to keep iterating, simplify the scope, or
   abandon and proceed without saving.

Do NOT silently accumulate fixes hoping it converges.

## What "tested" means

A validated skill has had its documented procedure invoked with
synthetic parameters in the same session, with all bundled
scripts running to completion and producing the expected output
shape. Specifically:

- All `${SKILL_DIR}/scripts/*` referenced in body actually exist
  and are runnable.
- All tools invoked are admitted by `allowed-tools` (no
  permission-denied responses).
- Script outputs (file paths, JSON, etc.) match what subsequent
  steps consume.
- The final user-facing artefact (report file, summary text,
  whatever the body promises) is produced.

Anything less and you should not say "tested, ready" to the user.
