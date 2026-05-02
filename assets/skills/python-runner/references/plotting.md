# Plotting (matplotlib + plotly)

The runtime sets `MPLBACKEND=Agg` — matplotlib runs **headless**.
`plt.show()` is a no-op; **always `savefig`**. Same for plotly: render
to HTML or PNG, never to a window.

## When to pick which

| Need | Tool |
|------|------|
| Quick static PNG/SVG to embed in a report or hand to the user | `matplotlib` |
| Interactive HTML chart (zoom, hover, legend toggle) | `plotly` |
| Map / spatial layer | `folium` (see `visualization` ref) |
| Data table next to a chart in an HTML report | `great_tables` (see `visualization` ref) |

## matplotlib — static charts

### Bar chart, saved as PNG
```python
import os, matplotlib.pyplot as plt, pandas as pd
SESSION = os.environ["SESSION_DIR"]
df = pd.read_parquet(os.path.join(SESSION, "data/sales.parquet"))

fig, ax = plt.subplots(figsize=(8, 4))
df.groupby("region")["amount"].sum().sort_values().plot.barh(ax=ax)
ax.set_xlabel("Total amount")
ax.set_title("Sales by region")

out = os.path.join(SESSION, "figures/sales_by_region.png")
os.makedirs(os.path.dirname(out), exist_ok=True)
fig.savefig(out, dpi=150, bbox_inches="tight")
plt.close(fig)              # release memory; the next call gets a fresh process anyway
print(out)
```

### Line + secondary axis (time series with rolling avg)
```python
fig, ax1 = plt.subplots(figsize=(10, 4))
df.plot(x="ts", y="amount", ax=ax1, label="daily", color="C0")
df.assign(ma=df["amount"].rolling(7).mean()).plot(
    x="ts", y="ma", ax=ax1, label="7-day MA", color="C3"
)
ax1.set_xlabel("date")
fig.savefig(os.path.join(SESSION, "figures/timeseries.png"), dpi=150, bbox_inches="tight")
plt.close(fig)
```

### Multi-panel grid
```python
fig, axes = plt.subplots(2, 2, figsize=(10, 8))
for ax, (name, sub) in zip(axes.flat, df.groupby("region")):
    sub["amount"].hist(ax=ax, bins=30)
    ax.set_title(name)
fig.tight_layout()
fig.savefig(os.path.join(SESSION, "figures/by_region_hist.png"), dpi=150)
plt.close(fig)
```

### Geometry (geopandas) — works headless
```python
import geopandas as gpd
gdf = gpd.read_parquet(os.path.join(SESSION, "data/cities.parquet"))
fig, ax = plt.subplots(figsize=(8, 8))
gdf.plot(ax=ax, column="population", legend=True, cmap="viridis")
ax.set_axis_off()
fig.savefig(os.path.join(SESSION, "figures/cities.png"), dpi=150, bbox_inches="tight")
plt.close(fig)
```

### Common matplotlib pitfalls
- `plt.show()` does nothing — you'll see a blank PNG if you forgot the
  `savefig`.
- Don't reuse `fig`/`ax` across calls — every `run_code` is a new process.
- `bbox_inches="tight"` trims whitespace; without it titles get clipped.
- `plt.close(fig)` after each save — without it the figure stays in
  matplotlib's registry until the process exits (fine for a single
  figure, leaks for loops).

## plotly — interactive HTML

### One chart → standalone HTML
```python
import os, plotly.express as px, pandas as pd
SESSION = os.environ["SESSION_DIR"]
df = pd.read_parquet(os.path.join(SESSION, "data/sales.parquet"))

fig = px.bar(df, x="region", y="amount", color="channel",
             title="Sales by region and channel")
out = os.path.join(SESSION, "reports/sales.html")
os.makedirs(os.path.dirname(out), exist_ok=True)
fig.write_html(out, include_plotlyjs="cdn", full_html=True)
print(out)
```

`include_plotlyjs="cdn"` keeps the file small (≈ 30 KB instead of 4 MB) —
the browser pulls plotly.js from a CDN. Use `"inline"` only when the user
needs an offline file.

### Embed plotly inside a bigger HTML report
Render to a fragment, splice it into your own HTML:

```python
fragment = fig.to_html(include_plotlyjs="cdn", full_html=False)
html = f"<html><body><h1>Sales</h1>{fragment}</body></html>"
open(os.path.join(SESSION, "reports/sales.html"), "w").write(html)
```

See the `visualization` reference for full report assembly with
`great_tables` + `folium` + plotly.

### plotly to PNG
```python
fig.write_image(os.path.join(SESSION, "figures/sales.png"), scale=2)
```

`write_image` requires `kaleido` — already in the bundled venv. No browser
spin-up.

### Common plotly pitfalls
- `fig.show()` does nothing — same as matplotlib. Always `write_html` or
  `write_image`.
- Big DataFrames render slowly in the browser. Aggregate or sample
  before plotting (`df.sample(n=10000)` for >100k rows).
- `px.line` over a non-sorted x-axis draws zigzags. Sort first:
  `df = df.sort_values("ts")`.

## Saving the script for re-runs

If the user might want to tweak axes / colours / sample size, write the
code to a file and tell them which path to edit:

```
1. bash.write_file({path: "scripts/plot_sales.py", content: ...})
2. python-mcp:run_script({path: "scripts/plot_sales.py"})
3. Surface "scripts/plot_sales.py" + "figures/sales_by_region.png"
```

The user re-runs the script, the figure regenerates — no need to re-derive
the code.
