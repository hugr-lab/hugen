---
name: report-builder
license: Apache-2.0
description: >
  Build a user-facing report / document (HTML, Markdown, PDF) from
  ALREADY-COLLECTED data — charts, publication tables, KPIs, prose,
  maps. Python-first: write a SHORT script that loads the data file,
  builds the figures, assembles the document, and writes it to disk —
  so the model never has to stream a multi-KB document inline (the
  slow path that wedges a stalling backend). Follows a report spec
  (sections, format, key metrics) when one is given. Renders with the
  bundled venv (plotly, great_tables, pandas, duckdb, folium,
  weasyprint) — the deep library recipes live in `python-runner`.
  Use this AFTER the data exists (a data file, or small inline data);
  to FETCH data load `hugr-data`, for SQL over files load
  `duckdb-data`.
allowed-tools:
  - provider: python-mcp
    tools:
      - run_code
      - run_script
  - provider: bash-mcp
    tools:
      - bash.write_file
      - bash.read_file
      - bash.list_dir
metadata:
  hugen:
    # python-runner carries the library recipes (plotting /
    # visualization references + the run_code/run_script envelope).
    # Requiring it makes `skill:ref(skill="python-runner", …)`
    # reachable; this skill adds only the report METHOD on top.
    requires_skills: [python-runner]
    autoload: false
    autoload_for: []
    # PLAIN render-only skill — the reusable report METHOD (python-
    # first, normalize-to-file, charts / html-generation). It is the
    # render half ONLY; FETCHING data is never its job.
    #
    # Loaded three ways, all worker / mission tier:
    #   - the analyst `report-builder` role autoloads it (the
    #     mission's render stage);
    #   - `_task_builder` author workers reach it via
    #     `skill:catalog_list` → `skill:load` when minting a report
    #     task (and the minted report task `requires_skills` it);
    #   - the `build_report` task `requires_skills` it for the render
    #     method (and `skill:ref`s its references).
    #
    # NOT task-eligible (deliberate). The standalone "render a report
    # from known data / a named query" path is the SEPARATE
    # `build_report` task (task-eligible, fetch-aware, one-script).
    # Keeping report-builder a plain skill is what lets the mission
    # autoload AND the task `requires_skills` reuse the SAME render
    # method instead of duplicating it. Not root-loadable: at root the
    # report entry point is the `build_report` task (render-ready data
    # / a named query) or the `analyst` mission (open-ended analysis) —
    # there is no report skill to load.
    tier_compatibility: [worker, mission]
compatibility:
  model: any
  runtime: hugen
---

# report-builder

Turn ready data into a user-facing **document**: a self-contained
HTML one-pager, a Markdown report, or a PDF — with charts,
publication-quality tables, KPI headers, and prose. The data is
already collected (a file you were given, or small inline data in
your inputs); your job is the **render**, not the fetch.

## Python-first — the default, and why

**Default: write a short Python script that builds the document and
writes the file.** NOT: emit the whole HTML document as a
`bash.write_file` argument token-by-token.

The reason is reliability, not taste. Emitting a multi-KB document
inline is a long, uninterrupted generation; on a slow / local backend
that stream can stall mid-document, and a stalled stream that already
started is **un-retryable** (re-issuing it would duplicate the tokens
already written). A short script — ~30 lines of plotly + great_tables
— keeps *your* generation short (narrow stall window, usually
retryable) and pushes the heavy document assembly into Python, a
deterministic step with zero stall exposure. The file lands the same
either way; one path is fragile, the other isn't.

**Inline document generation is the exception, not the rule.** It is
fine ONLY for a trivial report — a handful of numbers in a paragraph,
no charts, a few hundred bytes. The moment the document carries a
chart, a real table, or more than a screenful of prose, go through
Python. When in doubt, script it.

## The method

Keep each `run_code` / `run_script` call SHORT. Do not assemble the
whole report in one giant call; build it up.

0. **Read the report shape.** If you were given a report spec — a
   filled spec file or sections in your inputs — read it: it carries
   audience, ordered sections, format, key metrics, output path. Build
   EXACTLY those sections in that order. Absent, derive the shape from
   the task you were given + your inputs.

1. **Normalize every data input so python can load it — this is YOUR
   responsibility.** Python runs in a separate process and cannot see
   your prompt, so a dataset sitting in your inputs as text is not yet
   usable. Whatever shape your data arrived in, make it
   python-loadable. Three cases:
   - **A file path** in your inputs → use it as-is:
     `pd.read_parquet` / `pd.read_json` / `duckdb`.
   - **Small inline data** in your inputs (scalar KPIs; a series /
     table up to a few dozen rows) → use it directly as a literal in
     your script. Quote values VERBATIM, never round. No file needed —
     re-emitting a small literal is a short, safe generation.
   - **Large inline data** in your inputs (big distributions, many
     rows) → **write it to a workspace file FIRST** (`bash.write_file`;
     if it is big, chunk it with `mode="append"` — each append returns
     `size_total`, so a stall loses only the current chunk), THEN load
     that file. Do NOT embed a large dataset into the document
     generation — that is the long, un-retryable stream that stalls.
   After this step every dataset is either a small literal or a file,
   so the render below never re-emits a large dataset. Owning this
   yourself is what makes you self-sufficient — you do NOT depend on
   whoever produced the data having written it to a file.

2. **Load + build the figures** — read the file(s) into DataFrames;
   print `.shape` + `.head()` to confirm columns before you chart.
   Build plotly figures + great_tables tables, each rendered to an
   HTML fragment (`fig.to_html(full_html=False,
   include_plotlyjs="cdn")`, `GT(df).as_raw_html()`), in small
   batches — print a length check, not the fragment.
3. **Assemble** — splice the fragments into a template you control
   (the self-contained-HTML pattern in
   `skill:ref(skill="python-runner", ref="visualization")`), write the
   file with Python's own `open(...).write(...)` from inside the
   script. Do NOT route the document back through the model.
4. **Write to the right path** (see discipline below) and
   **verify**: `os.path.getsize(out) > 0` and `os.path.exists(out)`
   inside the script; print the absolute path and the byte count.
5. **Hand off** the path + byte count (shape below). Never claim a
   file you didn't verify on disk.

## File-path discipline

- If `inputs.file_path` (or `inputs.output_path`) is set, it is
  **literal** — it came from the user's request. Resolve to
  absolute via `os.path.expanduser` + `os.path.abspath` and write
  THERE. Never silently substitute a different path.
- If absent, write to a sensible default under the workspace
  (`<workspace>/<short-name>.<ext>`) and quote the absolute path you
  used. Do NOT prompt the user.
- **Relative-path rule vs the deliverable.** python-runner says "write
  outputs under `${SESSION_DIR}`" — that keeps the run_script *script
  file* and scratch inside the workspace. The user deliverable is the
  exception: the running script MAY write the rendered document to the
  resolved ABSOLUTE `inputs.file_path` (the process can write any path
  the OS permits — only the `.py` script's own location must be
  relative). If that absolute write fails (permission), fall back to
  `<workspace>/reports/<name>.<ext>` and report THAT path in your
  result so the user can still retrieve it.
- After the write: the file exists AND `size > 0` BEFORE you report it.

## Format

- **HTML** (default) — self-contained one-pager, plotly charts +
  great_tables tables. The end-to-end pattern + the chunked-write
  fallback for a genuinely huge document are in
  `skill:ref(skill="report-builder", ref="html-generation")`.
- **Markdown** — pipe tables, Mermaid fenced blocks for diagrams,
  sibling-image or base64 charts. Cheap and stall-immune (short
  output) — a natural default for a "quick report".
- **PDF** — render the HTML, then `weasyprint` HTML→PDF. WeasyPrint
  runs no JS, so embed charts as PNG (`fig.write_image`), not plotly
  HTML. See the `visualization` reference.

Picking a chart / table for a given data shape:
`skill:ref(skill="report-builder", ref="charts")`. The library code
(plotly, great_tables, folium, weasyprint) lives in python-runner's
`plotting` + `visualization` references — read those for the API; this
skill is the report METHOD and the shape decisions.

## What to report back

Report your result as:

```
{ path: "<absolute path you VERIFIED exists>",
  bytes_written: <int>,
  format: "html" | "md" | "pdf",
  sections: ["<section title>", ...],
  memory_summary: "<one line>" }
```

`path` is the absolute file you confirmed on disk — surface it
verbatim so the user can find it. If you could not produce the file
(a data input was missing or empty, the spec was unusable), report an
error with one sentence on the blocker — never report a path you
didn't write.
