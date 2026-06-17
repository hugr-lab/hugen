---
name: build_report
license: Apache-2.0
description: >
  Build a user-facing report / document (HTML, Markdown, PDF) from
  KNOWN data in one shot. Sources: inline values, data files, and/or
  GraphQL queries fetched in-script via hugr-client. Threads reusable
  `params` into the queries and the report. Reuses the report-builder
  render method (python-first, charts, publication tables). Use when
  you can name the data / the query AND the report shape; for
  open-ended analysis use the `analyst` mission instead.
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
    # report-builder carries the render METHOD (python-first,
    # normalize-to-file, charts / html-generation) and transitively
    # pulls python-runner (run envelope + hugr-client reference). This
    # task adds only the input model + the fetch-aware one-script flow
    # on top — the render itself is `skill:ref(report-builder, ...)`,
    # never duplicated here.
    requires_skills: [report-builder]
    autoload: false
    autoload_for: []
    tier_compatibility: [worker]
    task:
      eligible: true
      kind: worker
      goal_summary: >
        Render a finished report / document (HTML, Markdown, or PDF)
        from data that is ALREADY known — passed inline, given as data
        files, or named as GraphQL queries to fetch. Fetches each
        query and renders in one script. Use when the data source and
        the output shape are given; this is rendering, not
        investigation.
      inputs_schema:
        type: object
        required: [report]
        properties:
          params:
            type: object
            description: >
              Reusable values threaded into the queries (as GraphQL
              variables) and into the report (title, labels, filters).
              Free-form key/value, e.g. {"region_id": "84", "year": 2025}.
          queries:
            type: array
            description: >
              GraphQL queries to fetch data IN-SCRIPT via hugr-client
              (no extra tool grant — HUGR_URL/HUGR_TOKEN are injected
              into the run). Each result becomes a named dataset.
            items:
              type: object
              required: [name, graphql]
              properties:
                name:
                  type: string
                  description: Dataset name to reference when building the report.
                graphql:
                  type: string
                  description: GraphQL query text.
                variables:
                  type: object
                  description: Per-query variables; may reference params.
                part_path:
                  type: string
                  description: >
                    Response part path, e.g. "data.<field>" (deeper for
                    nested modules). Omit when the query selects one field.
          data_files:
            type: array
            description: Existing data files (parquet / json / csv) to load.
            items:
              type: object
              required: [name, path]
              properties:
                name:
                  type: string
                  description: Dataset name to reference when building the report.
                path:
                  type: string
                  description: Absolute or workspace-relative file path.
          data:
            type: object
            description: >
              Small inline datasets keyed by name — a handful of KPIs or
              up to a few dozen rows. Quote values verbatim; never round.
          report:
            type: object
            required: [format]
            properties:
              title:
                type: string
                description: Document title.
              format:
                type: string
                enum: [html, md, pdf]
                description: Output document format. Default html.
              sections:
                type: array
                items:
                  type: string
                description: Ordered section titles to build, in order.
              spec_path:
                type: string
                description: Path to a report-spec file; overrides sections when set.
              language:
                type: string
                description: Output language, e.g. "ru" or "en".
              output_path:
                type: string
                description: >
                  Absolute path to write the document. Default: a
                  sensible name under the workspace.
      allowed_tools_default:
        - python-mcp:run_script
        - python-mcp:run_code
        - bash-mcp:bash.write_file
        - bash-mcp:bash.read_file
        - bash-mcp:bash.list_dir
compatibility:
  model: any
  runtime: hugen
---

# build_report

Render a finished **document** — an HTML one-pager, a Markdown report,
or a PDF — from data that is already known. The sources and the shape
are given to you; your job is to fetch (when a query is named), load,
and render. This is NOT an investigation: you do not explore the
schema or decide what to analyse — that is the `analyst` mission.

The render METHOD — python-first, normalize-every-input-to-a-file,
chart / table selection, the self-contained-HTML pattern, the PDF
path — lives in `report-builder`, which is loaded for you. Read it
with `skill:ref(skill="report-builder", ref="html-generation")` /
`ref="charts"` and follow it. This skill adds only the **input model**
and the **fetch-aware, one-script** flow on top.

## Inputs

`{ params?, queries[]?, data_files[]?, data?, report }`. At least one
data source (`queries` / `data_files` / `data`) unless the report is
pure prose. `params` is the reusable variable surface — thread it into
query `variables` AND into report titles / labels / filters.

## Method — one script when single-source

The fast path is the point of this task: for a single source, write
ONE short `run_script` that fetches (or loads), builds the figures,
assembles the document, and writes the file. For a genuinely big or
multi-source report, split into a fetch+normalize script then a render
script — but keep each `run_script` short (a stalled long generation
is un-retryable; see the report-builder method).

1. **Normalize every source to a python-loadable dataset** (the
   report-builder method — make each input a DataFrame or a small
   literal; large inline data goes to a file FIRST). The source decides
   the route — never re-fetch data you were handed:
   - **A query** (`queries[]`) → fetch in-script with `hugr-client`.
     Re-read `HUGR_URL` / `HUGR_TOKEN` from the env every call (never
     cache the token); see
     `skill:ref(skill="python-runner", ref="hugr-client")`. Use the
     provided `graphql` text **VERBATIM** — assign it to a string
     literal exactly as given; do NOT reformat, re-indent, or retype
     it (reflowing a nested query drops braces and breaks it). Any
     GraphQL error — a syntax error, an unknown field, a bad argument —
     makes `query()` **raise `ValueError`**; an RBAC-filtered or
     genuinely empty result, by contrast, is NOT an error and returns
     normally with `r.parts == {}`. Always guard the fetch for both:

     ```python
     from hugr import HugrClient
     c = HugrClient()                          # reads HUGR_URL / HUGR_TOKEN from the env
     try:
         r = c.query(gql, variables=vars)      # gql verbatim; vars from params
     except (ValueError, PermissionError) as e:  # GraphQL errors / HTTP 500 / 401-403
         raise SystemExit(f"query failed: {e}")
     if not r.parts or part_path not in r.parts:  # empty / RBAC-filtered (not an error → no raise)
         raise SystemExit(f"no data for {part_path}; parts={list(r.parts)}")
     df = r.df(part_path)                        # or r[part_path].to_arrow()
     ```

     `df(path)` / `record(path)` / `gdf(path, field)` are on the
     response; `to_arrow()` is on the PART (`r[part_path].to_arrow()`),
     takes no path. Both checks matter: a malformed / errored query
     RAISES (caught by `except`), while an empty / RBAC-filtered result
     returns `r.parts == {}` with no exception — without both you'd
     debug a "fetch bug" that is really a bad or empty query; surface it
     as a blocker instead.
   - **A file** (`data_files[]`) → `pd.read_parquet` / `read_json` /
     `duckdb` by `path`.
   - **Inline** (`data`) → use as a literal in the script; quote
     values verbatim, no rounding.
2. **Build + assemble + write.** Build the figures / publication
   tables, splice them into the template you control, and write the
   document to `report.output_path` (resolve to absolute; default to a
   workspace path and quote it). Honour `report.sections` /
   `report.spec_path` in order, plus `report.format` and
   `report.language`. Verify on disk: `os.path.exists(out)` and
   `os.path.getsize(out) > 0` BEFORE you report it.

## What to report back

```
{ path: "<absolute path you VERIFIED exists>",
  bytes_written: <int>,
  format: "html" | "md" | "pdf",
  sections: ["<section title>", ...],
  memory_summary: "<one line>" }
```

`path` is the absolute file you confirmed on disk — surface it
verbatim. If you could not produce the file (a query failed, a source
was empty, the spec was unusable), report an error with one sentence
on the blocker — never report a path you didn't write. You have no
discovery tools: a failing query is a blocker to report, not a schema
to investigate (that is the `analyst` mission's job).
