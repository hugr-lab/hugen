# HTML generation — the python-first method, end to end

The reliable recipe for an HTML report: **render each block as an HTML
fragment in Python, splice the fragments into a template you control,
write the file once from inside the script.** The model never streams
the document; Python assembles it.

The full library code (plotly `to_html`, `great_tables.as_raw_html`,
folium `iframe srcdoc`, weasyprint HTML→PDF, the self-contained
template) lives in
`skill:ref(skill="python-runner", ref="visualization")` and
`skill:ref(skill="python-runner", ref="plotting")`. Read those for the
API. This reference is the *method* — how a report-builder sequences
those pieces without wedging.

## First: normalize your data so python can load it

Before any rendering, make sure every dataset is python-loadable —
python can't read your prompt context. Per the SKILL.md method:
a **file path** you load as-is; **small inline** data you embed as a
literal; **large inline** data you write to a workspace file FIRST
(`bash.write_file`, `mode="append"` to chunk a big one) and then load.
The render below assumes the data is already a file or a small literal,
so it never re-emits a large dataset.

## Skeleton (load → fragments → assemble → write → verify)

Build this up in SHORT `run_code` / `run_script` calls — never one
giant call that both computes every figure AND emits the whole
document.

```python
import os, pandas as pd, plotly.express as px
from great_tables import GT

SESSION = os.environ["SESSION_DIR"]

# 1. LOAD — the file you were given, or the one you normalized from
#    inline data (see "normalize" above). Never paste rows here.
df = pd.read_parquet(os.path.join(SESSION, "data/op2023.parquet"))
print(df.shape, list(df.columns))            # confirm columns, no dump

# 2. FRAGMENTS — one figure / table at a time, each → an HTML string.
summary = df.groupby("region", as_index=False)["amount"].sum()
tbl_html  = GT(summary).tab_header(title="By region").as_raw_html()
chart_html = px.bar(summary, x="region", y="amount").to_html(
    include_plotlyjs="cdn", full_html=False, div_id="c-region")
print(len(tbl_html), len(chart_html))        # length check, not the body

# 3. ASSEMBLE — splice into a template you own. Sections come from
#    report-spec.md, in that order.
html = f"""<!doctype html><html><head><meta charset="utf-8">
<title>Operations 2023</title>
<style>body{{font-family:sans-serif;max-width:960px;margin:2em auto}}
section{{margin:2em 0}}h1{{border-bottom:2px solid #333}}</style>
</head><body>
<h1>Operations 2023</h1>
<section><h2>Summary</h2>{tbl_html}</section>
<section><h2>By region</h2>{chart_html}</section>
</body></html>"""

# 4. WRITE — Python writes the file directly; the document never goes
#    back through the model. The .py script lives under the workspace
#    (relative), but it MAY write the deliverable to an ABSOLUTE user
#    path — only the script file's own location is constrained, not
#    what it writes. Substitute inputs.file_path here when one was
#    given; otherwise a workspace path.
OUT = "/Users/.../op2023.html"   # ← inputs.file_path, or reports/op2023.html
out = os.path.expanduser(os.path.abspath(OUT))
os.makedirs(os.path.dirname(out), exist_ok=True)
with open(out, "w") as f:
    f.write(html)

# 5. VERIFY — prove it landed before you report it.
assert os.path.exists(out) and os.path.getsize(out) > 0, "empty/missing output"
print(out, os.path.getsize(out))
```

Hand off `out` + the byte count. The path you print is what the
synthesizer surfaces.

## Why python-first (the wedge, in one paragraph)

Emitting the whole document as a `bash.write_file` argument is a long,
uninterrupted model generation. On a slow / local backend that stream
can stall mid-document — and a stream that already started writing is
**un-retryable**, because re-issuing it would duplicate the tokens
already sent. The script above keeps the model's own output to ~30
short lines (a narrow stall window, usually still retryable) and moves
the multi-KB assembly into Python, which never stalls. Same file, far
fewer dead runs.

## The inline escape hatch (and its fallback)

For a **trivial** report — a few numbers in a paragraph, no charts —
you may emit the small HTML inline via `bash.write_file`. The line is
sharp: no chart, no real table, a few hundred bytes. Anything past
that goes through the script.

If you are forced down the inline path for a genuinely large document
(no Python available, an odd constraint), write it in **resumable
chunks** rather than one call: `bash.write_file(path, first_chunk)`
then `bash.write_file(path, content=next_chunk, mode="append")` per
section. Each chunk is a SEPARATE tool call, so a stall mid-chunk
loses only that chunk — the sections already appended are durable on
disk. Each append returns `size_total` (the file's full size so far),
so on retry you know exactly how much landed and re-issue only the
missing tail. This is a fallback, not the plan — prefer the script.

## Hard limits: 10000 bytes per write_file / run_code call

Both `bash.write_file` (content) and `python:run_code` (code) cap a
single call at **10000 bytes** — a longer single generation is
wedge-prone (the model streams it token-by-token and can stall mid-
stream). So:

- **Never embed a large dataset / full schema as a literal** in a
  `run_code` script. If the script would exceed 10 KB because it
  carries the data inline, you have the wrong shape: write the data to
  a file (it is usually already one — load it with `pd.read_*`), so
  the script stays a small `load → build → write`.
- **A genuinely large script** (lots of figures/sections) → write the
  `.py` to the workspace with `bash.write_file` in ≤10 KB **append**
  chunks, then run it with `run_script` — execution streams nothing.
- **A large inline document** → the chunked `mode="append"` writes
  above (≤10 KB each).

The cap is the runtime forcing the discipline; the goal is the same as
everywhere — keep the data in files, keep each generation short.

## Common failure modes

- **You loaded the data into the prompt instead of the file.** If your
  inputs carried a `path`, load it with pandas — do not re-paste rows
  a summary already mentioned. Pasting the dataset back inline is
  exactly what bloats the turn and invites the stall.
- **One mega-call.** Computing every figure and assembling the whole
  document in a single `run_code` defeats the point — the model still
  emits the full document string. Split: fragments in one call,
  assemble + write in the next.
- **Unverified result.** You reported a path you never `getsize`-d.
  Always assert existence + size > 0 inside the script.
- **Plotly blank in PDF.** WeasyPrint runs no JS — embed charts as PNG
  (`fig.write_image`) for the PDF path. See the `visualization`
  reference.
