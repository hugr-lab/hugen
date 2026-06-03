# Charts & tables — picking the shape for the data

This reference is the **decision guide**: which chart / table for which
data shape. The library code (plotly, great_tables, folium) lives in
python-runner's `plotting` + `visualization` references — read those
for the API; read this to choose.

## Chart by data shape

| You have | Use | Notes |
|----------|-----|-------|
| One value over a category (sales by region) | **bar** | `px.bar`; sort descending unless the category has a natural order |
| One value over time (daily revenue) | **line** | `px.line`; keep the x axis a real datetime, not a string |
| Part-to-whole, ≤6 parts | **bar (stacked)** or a labelled table | avoid pie charts — a sorted bar reads faster |
| Two numerics, correlation | **scatter** | `px.scatter`; add `trendline="ols"` only if the relationship is the point |
| Distribution of one numeric | **histogram** / **box** | `px.histogram` for shape, `px.box` to compare groups |
| Category × category intensity | **heatmap** | `px.density_heatmap` or great_tables `.data_color` on a pivot |
| Many small multiples | **faceted** chart | `facet_col=` / `facet_row=` — one call, a grid of panels |
| Geographic points / regions | **folium map** | see the `visualization` reference; embed via `iframe srcdoc` |

When the report-spec names a chart library or kind, follow it. Absent a
spec, pick the simplest shape that answers the section's question — a
sorted bar or a clean table beats a clever chart.

## Tables — great_tables, not raw HTML

For any table that goes INTO the report, use `great_tables.GT` and
`.as_raw_html()` — it gives proper headers, number formatting, and
heatmap colouring with a fluent API. Reach for it whenever a section is
"a table of X":

- `.tab_header(title, subtitle)` — the table's caption.
- `.fmt_number / fmt_percent / fmt_currency / fmt_date` — format
  columns; never hand-format numbers in Python strings.
- `.data_color(columns, palette)` — heatmap a metric column.
- `.cols_label(...)` / `.tab_spanner(...)` — readable headers + grouping.

Quote the numbers verbatim from the data file — formatting (separators,
decimals, percent) is great_tables' job, not a paraphrase. The full API
+ examples are in `skill:ref(skill="python-runner", ref="visualization")`.

## KPI headers

A report usually opens with a few headline numbers (total, growth,
count). Compute them in Python from the data file and splice them as a
small flex row of cards — a `<div>` per KPI with the number large and
the label small. Keep the values verbatim from the data; do not round
in prose what the table shows precisely.

## Keep it sectioned

The report-spec's `## Sections` is the table of contents — one
`<section>` per line, in that order, each with its `<h2>` and its one
or two figures / tables. A report is a sequence of small, titled
blocks, not one dense wall. Build each section's fragment, then splice
them in order (see `html-generation`).
