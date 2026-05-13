# H3 Hexagonal Spatial Aggregation

The `h3` top-level query aggregates data from any spatial data
object into H3 hexagonal cells. Use it to create density maps,
geoembeddings, or any analysis that needs a uniform spatial grid
across heterogeneous data sources.

> **Naming inside `data { ... }`**: examples in this file use
> short unprefixed names (`buildings_aggregation`,
> `county_boundaries_aggregation`, …) for readability — they
> assume a prefix-less catalog. **In a prefixed catalog, all
> identifiers inside `_spatial(...) { ... }`, `_join(...) { ... }`,
> and `h3(...) { data { ... } }` blocks must be flat prefixed
> type names** (e.g. `osm_buildings_aggregation`), because those
> blocks bypass module nesting. See `spatial-queries.md` §
> "Critical naming rule" for the full rule and a worked example.

The cell resolution (0–15) controls hexagon size:

| Resolution | Approx edge | Use case |
|------------|-------------|----------|
| 3–4        | ~25–60 km   | Country / state overview |
| 5–7        | ~1–10 km    | City-scale density |
| 8–9        | ~200–500 m  | Neighbourhood |
| 10–12      | ~10–70 m    | Street-level |

Lower resolution = fewer cells, faster query, coarser detail.
Start at 5–7 for exploratory work, refine only if the question
needs more grain.

## Basic shape

```graphql
query {
  h3(resolution: 6) {
    cell           # H3 cell id (string)
    resolution     # echoes the resolution argument
    geom           # optional — hexagon geometry for mapping
    data {
      # one or more *_aggregation calls live here
      buildings: buildings_aggregation(field: "footprint", inner: true) {
        _rows_count
        area_sqm { sum }
      }
    }
  }
}
```

Each entry in `data { ... }` is a normal `_aggregation` or
`_bucket_aggregation` call against a spatial data object, with
two extra knobs (see below).

## Aggregation knobs inside `data { ... }`

| Argument | Default | Meaning |
|----------|---------|---------|
| `field`  | required | geometry column to aggregate against |
| `inner`  | `false`  | `true` → skip H3 cells with no data |
| `divide_values` | `true` | proportionally split row values across cells the row overlaps. `false` → keep the original totals (use when the value is per-row, not per-area, e.g. census population per Landkreis) |
| `filter` | none     | pre-filter rows before the spatial aggregation |

`divide_values` is the most subtle setting:
- A residential **building** is small relative to most H3 cells
  — `divide_values: true` (default) makes its `area_sqm.sum` flow
  proportionally into whichever cell(s) the polygon falls in.
- A **Landkreis** (county) polygon spans many H3 cells. Setting
  `divide_values: false` keeps its `population` whole — every
  cell intersected reports the full county population (then you
  apportion it with `distribution_by`, below).

## `@stats` directive

Add `@stats` to the `h3` query to attach per-cell summary stats
to the result, useful for normalising / scoring:

```graphql
h3(resolution: 7) @stats {
  cell resolution
  data { ... }
}
```

## `distribution_by` — proportional value redistribution

Use when one aggregation gives you a "stock" value at a coarse
shape (counties, postal districts) and another gives you a
denominator you want to use to spread the stock proportionally
inside each H3 cell.

```graphql
h3(resolution: 6) @stats {
  cell resolution
  data {
    # 1. Coarse-grain numerator — keep totals whole.
    counties: county_boundaries_aggregation(
      field: "geom"
      filter: { admin_level: { eq: 6 } }
      divide_values: false
      inner: true
    ) {
      pop: _join(fields: ["county_code"]) {
        census_population(fields: ["county_code"]) {
          residents { sum }
        }
      }
    }
    # 2. Fine-grain denominator — let it split per cell.
    houses: building_footprints_aggregation(
      field: "geom"
      filter: { use: { eq: "residential" } }
    ) {
      _rows_count
      floor_area { sum }
    }
  }

  # 3. Redistribute county population across the cell, weighted
  #    by share of residential floor area.
  pop: distribution_by(
    numerator:   "data.counties.pop.census_population.residents.sum"
    denominator: "data.houses.floor_area.sum"
  ) {
    value
    ratio
    numerator
    denominator
    denominator_total
  }
}
```

Formula:

```
value = numerator * (denominator / denominator_total)
```

Where `denominator_total` is the sum of the denominator over the
**parent scope** of the numerator (in this example: total
residential floor area inside one county). Cells with no
residential buildings get `value = 0`.

## `distribution_by_bucket` — distribution into bucket categories

Same idea, but the denominator is a `_bucket_aggregation` and the
output preserves the bucket key. Use this when you want a
breakdown by category (building type, business sector, …) within
each H3 cell.

```graphql
h3(resolution: 6) @stats {
  cell resolution
  data {
    counties: county_boundaries_aggregation(
      field: "geom" filter: { admin_level: { eq: 6 } }
      divide_values: false inner: true
    ) {
      pop: _join(fields: ["county_code"]) {
        census_population(fields: ["county_code"]) {
          residents { sum }
        }
      }
    }
    houses_bucket: building_footprints_bucket_aggregation(
      field: "geom" filter: { use: { eq: "residential" } }
    ) {
      key { building_type }
      aggregations {
        _rows_count
        floor_area { sum }
      }
    }
  }

  pop_by_bucket: distribution_by_bucket(
    numerator:       "data.counties.pop.census_population.residents.sum"
    denominator_key: "data.houses_bucket.key"
    denominator:     "data.houses_bucket.aggregations.floor_area.sum"
  ) {
    denominator_key   # carries the bucket key forward (building_type)
    value
    ratio
  }
}
```

`numerator` may itself point at a bucket aggregation — Hugr will
align bucket keys on both sides.

## Cross-source patterns

`h3` composes with `_join` (dynamic joins) and `_spatial`. The
canonical population-density example joins OSM geometry to a
census table from a different data source via a shared
administrative code, all inside one `h3` cell aggregation. The
`_join` block lives **inside** the `data.X_aggregation { ... }`
selection, not at the top level.

## Decision sketch

```
Need geographic density / per-cell breakdown?
├── ONE source, just "how many X per cell"
│       → h3(resolution: …) { data { X_aggregation(...) } }
├── Stock value (population, sales) split by share of a finer var
│       → h3 { data { stock(divide_values:false) denom(...) }
│                pop: distribution_by(numerator, denominator) }
├── Same, but you want the breakdown by category per cell
│       → distribution_by_bucket(numerator, denominator_key, denominator)
└── Two different geometries that should overlap
        → use _spatial inside the aggregation (see spatial-queries.md)
```

## Critical rules

- Resolution choice dominates everything. A resolution-12 query
  over a whole country can return tens of millions of cells —
  always start lower and refine.
- `inner: true` is almost always what you want; `false` returns
  the entire H3 grid of the bounding region including empty cells.
- `divide_values` defaults to `true` and SILENTLY distributes
  per-row values. For census-like "totals at a coarse polygon"
  semantics, you MUST pass `divide_values: false` or the totals
  get diluted across cells.
- `distribution_by` paths are dotted strings into the `data` block
  above — typos return zeros, not errors. Copy paths exactly from
  the query you wrote.
- The `numerator` and `denominator` paths must terminate at a
  scalar (sum, avg, …), not at an object.

## See also

- `spatial-queries.md` — pairwise spatial joins (`_spatial`)
  without grid aggregation.
- `query-patterns.md` — `_join` for cross-source attribute joins.
- `aggregations.md` — `_aggregation` / `_bucket_aggregation`
  basics.
- Full canonical example with OSM + Zensus:
  https://hugr-lab.github.io/docs/examples/h3-spatial
