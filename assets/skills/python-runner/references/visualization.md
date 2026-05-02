# HTML / PDF reports + spatial visualization

The bundled venv carries the full report stack: `great_tables` for
publication-quality tables, `plotly` for interactive charts (covered in
`plotting`), `folium` for maps, `weasyprint` for HTML→PDF. Output goes
under `${SESSION_DIR}/reports/`.

## Choose your output type up front

| User asks for | Output | Stack |
|----------------|--------|-------|
| "Make me a one-pager I can open in a browser" | Self-contained HTML | `great_tables` + `plotly` + `folium` |
| "Send me a PDF" | PDF | `weasyprint` (HTML → PDF) **or** `matplotlib.savefig(*.pdf)` for chart-only |
| "Spatial / map view" | HTML with embedded map | `folium` (with chart annotations) |
| "Static chart-only PDF" | PDF | `matplotlib.savefig("...pdf")` — no HTML pipeline needed |

WeasyPrint requires Cairo / Pango / gdk-pixbuf / libffi on the host.
The operator installs those once (see README §Prerequisites). At runtime,
treat its absence as a hard error and fall back to a PNG +
`matplotlib.savefig("...pdf")` if you must produce a PDF.

## great_tables — publication-quality tables

Built for the "table that goes into the report" use case. Output is HTML
(or PNG) with proper formatting, headers, footers.

```python
import os, pandas as pd
from great_tables import GT, md, html

SESSION = os.environ["SESSION_DIR"]
df = pd.read_parquet(os.path.join(SESSION, "data/sales_summary.parquet"))

gt = (
    GT(df)
      .tab_header(title="Q1 sales", subtitle="By region and channel")
      .tab_source_note(md("Source: hugr `core.sales` aggregation"))
      .fmt_number(columns=["total"], decimals=0, sep_mark=" ")
      .fmt_percent(columns=["growth"], decimals=1)
      .data_color(columns=["growth"], palette=["#cc0000", "#ffffff", "#006600"])
)

# As an HTML fragment to splice into your own report
tbl_html = gt.as_raw_html()

# Or directly to file
gt.save(os.path.join(SESSION, "reports/sales_summary.html"))
```

Key methods:

- `.tab_header(title, subtitle)` — top of the table
- `.tab_source_note(...)` — footnote
- `.fmt_number / fmt_percent / fmt_currency / fmt_date` — column formatting
- `.data_color(columns, palette)` — heatmap colouring
- `.cols_label(col="Display Name")` — pretty headers
- `.tab_spanner(label, columns)` — group columns under a header
- `.tab_options(table_width="800px")` — global look-and-feel

Check the GT docstrings (`help(GT.fmt_number)` from `run_code`) for the
full surface — it's a fluent API and deeper than the snippet above.

## folium — interactive maps

`folium` builds Leaflet maps. Output is HTML; embed via `_repr_html_()` or
save with `.save()`.

### Basic point map
```python
import folium, os
SESSION = os.environ["SESSION_DIR"]

m = folium.Map(location=[55.75, 37.62], zoom_start=10, tiles="OpenStreetMap")
for _, row in df.iterrows():
    folium.CircleMarker(
        location=[row["lat"], row["lon"]],
        radius=4, popup=row["name"],
        color="#3388ff", fill=True, fill_opacity=0.7,
    ).add_to(m)

m.save(os.path.join(SESSION, "reports/cities.html"))
```

### GeoJSON layer (from a GeoDataFrame produced by hugr-client)
```python
import folium, geopandas as gpd

gdf = result.gdf("data.cities", "geom")  # from hugr-client
m = folium.Map(location=[55.0, 37.0], zoom_start=4)
folium.GeoJson(
    gdf.to_json(),
    name="cities",
    tooltip=folium.GeoJsonTooltip(fields=["name", "population"]),
).add_to(m)
folium.LayerControl().add_to(m)
m.save(os.path.join(SESSION, "reports/cities_map.html"))
```

### Embed a folium map inside a bigger report
`folium.Map._repr_html_()` returns the HTML representation; or use
`m.get_root().render()`:

```python
map_html = m.get_root().render()
```

Don't paste this into a string-template alongside other content unless you
strip the surrounding `<html>/<body>` — see "Self-contained HTML report"
below for the assembly pattern.

## weasyprint — HTML → PDF

```python
from weasyprint import HTML, CSS

HTML(string=html_string).write_pdf(
    os.path.join(os.environ["SESSION_DIR"], "reports/sales.pdf"),
    stylesheets=[CSS(string="@page { size: A4; margin: 1.5cm; }")],
)
```

Or render an existing HTML file:

```python
HTML(filename=os.path.join(SESSION, "reports/sales.html")).write_pdf(
    os.path.join(SESSION, "reports/sales.pdf")
)
```

WeasyPrint quirks:

- **Loads images lazily** — relative URLs are resolved against
  `base_url=`, default the cwd. If your HTML references
  `figures/sales.png`, set `base_url=os.path.join(SESSION, "reports")`.
- **No JavaScript.** Plotly's interactive widgets become static
  placeholders in the PDF. For PDF, use `fig.write_image(...png)` and
  embed the PNG, NOT the plotly HTML.
- **No external CDN if offline** — set `include_plotlyjs="inline"` in
  plotly when the PDF must work offline.
- **Custom fonts** must be on the host. Stick to system fonts unless the
  operator installed extras.

## Self-contained HTML report — assembly pattern

The reliable recipe: render each block as an HTML fragment, splice into a
template you control, write once.

```python
import os, pandas as pd, plotly.express as px
from great_tables import GT
SESSION = os.environ["SESSION_DIR"]

# 1. data
df = pd.read_parquet(os.path.join(SESSION, "data/sales.parquet"))
summary = df.groupby("region", as_index=False)["amount"].sum().sort_values("amount", ascending=False)

# 2. fragments
tbl_html = GT(summary).tab_header(title="Sales by region").as_raw_html()
chart_html = px.bar(summary, x="region", y="amount").to_html(
    include_plotlyjs="cdn", full_html=False, div_id="chart-region"
)

# 3. template
html = f"""<!doctype html>
<html><head>
  <meta charset="utf-8">
  <title>Q1 sales</title>
  <style>
    body {{ font-family: sans-serif; max-width: 960px; margin: 2em auto; padding: 0 1em; }}
    h1 {{ border-bottom: 2px solid #333; padding-bottom: .2em; }}
    section {{ margin: 2em 0; }}
  </style>
</head><body>
  <h1>Q1 sales</h1>
  <section><h2>Summary table</h2>{tbl_html}</section>
  <section><h2>Chart</h2>{chart_html}</section>
</body></html>"""

# 4. write
out = os.path.join(SESSION, "reports/q1_sales.html")
os.makedirs(os.path.dirname(out), exist_ok=True)
with open(out, "w") as f:
    f.write(html)
print(out)
```

For the same content as PDF, swap step 4:

```python
from weasyprint import HTML
HTML(string=html, base_url=os.path.join(SESSION, "reports")).write_pdf(
    os.path.join(SESSION, "reports/q1_sales.pdf")
)
```

— but replace the plotly chart with a PNG (`fig.write_image(...)`) before
PDF rendering, since WeasyPrint doesn't run JS.

## Map dashboard — folium + summary table

```python
import folium, pandas as pd, os
from great_tables import GT
SESSION = os.environ["SESSION_DIR"]

m = folium.Map(location=[55.0, 37.0], zoom_start=4)
folium.GeoJson(gdf.to_json(), name="cities").add_to(m)
map_html = m.get_root().render()    # full <html> doc — strip if embedding

tbl_html = GT(top10_df).as_raw_html()

# Two-column layout: map left, table right
html = f"""<!doctype html><html><head><meta charset="utf-8">
<style>
  .row {{ display: flex; gap: 1em; }}
  .col-map {{ flex: 2; min-height: 600px; }}
  .col-table {{ flex: 1; }}
  iframe.map {{ width: 100%; height: 100%; border: 0; }}
</style></head><body>
<h1>Top 10 cities</h1>
<div class="row">
  <div class="col-map">
    <iframe class="map" srcdoc='{map_html.replace("'", "&apos;")}'></iframe>
  </div>
  <div class="col-table">{tbl_html}</div>
</div>
</body></html>"""

with open(os.path.join(SESSION, "reports/map_dashboard.html"), "w") as f:
    f.write(html)
```

For maps inside a single-document report, an `<iframe srcdoc=...>` keeps
folium's CSS isolated from your outer styles.

## Files to surface, in order

After the report is written, the user sees only what you tell them.
Always surface the relative path:

> Built `reports/q1_sales.html`. Open it in a browser to interact with the
> chart; for a print copy see `reports/q1_sales.pdf`.

Never paste absolute paths — the host's view of `${SESSION_DIR}` differs
from the path inside the child process.

## Common pitfalls

- **PDF shows a blank chart** — WeasyPrint can't run plotly JS. Render
  the chart to PNG via `fig.write_image(...)` and embed an `<img>` tag.
- **HTML opens to a "ChunkLoadError"** — `include_plotlyjs="cdn"` failed
  to fetch (offline). Re-render with `include_plotlyjs="inline"`.
- **Folium map renders blank** — usually a missing `location=` or
  `zoom_start=`; or you embedded a full-document folium HTML directly
  (without iframe). Use `m.save(file)` for standalone, `iframe srcdoc`
  for embedded.
- **`great_tables` raises `ImportError: No module named 'IPython'`** — a
  cosmetic warning on some old great_tables versions. The bundled venv
  pins a fixed version; if you see it the operator's requirements list is
  out of date.
- **WeasyPrint segfaults on macOS** — host is missing Pango/Cairo. Tell
  the user; surface the operator README link. Don't retry.
