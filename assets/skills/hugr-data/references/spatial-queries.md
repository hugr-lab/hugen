# Spatial Queries — `_spatial`

The `_spatial` field is available on every data object that has a
geometry column. It joins this object to a spatially related
object via one of five geometry predicates, and is the primary
way to express geographic relationships in Hugr GraphQL.

Always pair `_spatial` with a paged or aggregated nested selection
— raw spatial joins on large datasets are the #1 source of
runaway queries.

## Argument shape

```graphql
_spatial(
  field:  "<source geometry column>",     # required
  type:   INTERSECTS | WITHIN | CONTAINS | DISJOINT | DWITHIN,
  buffer: 5000                            # meters, required iff type == DWITHIN
) {
  <prefix>_<object>(field: "<their geometry col>") { ... }
  <prefix>_<object>_aggregation(field: "...") { ... }
  <prefix>_<object>_bucket_aggregation(field: "...") { ... }
}
```

The OUTER `field:` names the geometry column on the parent row;
the INNER `field:` names the geometry column on the joined object.
Both arguments are required because Hugr does not infer geometry
columns — a data object may have several (`point`, `boundary`,
`coverage_area`, …).

### Naming rule — top-level vs `_spatial` body

Hugr has TWO contexts for naming a data object, and they look
different. Mixing them is the most common cause of
`Cannot query field "X" on type "Y"` errors with `_spatial`.

**Top-level** (anywhere outside `_spatial / _join / h3.data`):
- The path is **module nesting** mirroring the dotted module
  name, ending in the **unprefixed query field name**.
- Module `nw` → `nw { customers { ... } }`.
- Module `osm.bw` → `osm { bw { osm_amenities { ... } } }`.
- The unprefixed name is what `discovery-search_module_data_objects`
  returns under `queries[].name` (e.g. `osm_amenities`,
  `osm_amenities_aggregation`, `osm_amenities_bucket_aggregation`).

**Inside `_spatial { ... }` / `_join { ... }` / `h3 > data { ... }`:**
- There is NO module nesting — the namespace is flat.
- Use the **prefixed type name** of the target object.
- The prefixed name is what `discovery-search_module_data_objects`
  returns under `name` (e.g. `osm_bw_osm_amenities`,
  `osm_bw_osm_amenities_aggregation`). It is also the value you
  pass to `schema-type_fields(type_name: "...")`.

```graphql
# WRONG shapes:
query {
  nw_customers { ... }                                     # ✘ top-level + prefixed (no module path)
  nw {
    customers {
      _spatial(field: "location", type: WITHIN) {
        nw { customers(field: "geom") { id } }             # ✘ module nesting inside _spatial
        customers(field: "geom") { id }                    # ✘ unprefixed inside _spatial (only works if catalog has no prefix)
      }
    }
  }
}

# RIGHT — module nesting outside, flat prefixed inside:
query {
  nw {                                  # ← module nesting
    customers {                         # ← unprefixed query field name
      id name location
      _spatial(field: "location", type: DWITHIN, buffer: 5000) {
        nw_customers(field: "geom") {   # ← prefixed type name, flat
          id
        }
        nw_customers_aggregation(field: "geom") {           # ← same pattern
          _rows_count
        }
      }
    }
  }
}
```

If the schema has no prefix on this data source (catalogs without
a `prefix` setting), the bare object name happens to match both
forms — but always copy the exact names from discovery rather
than guessing. When the prefix exists, getting it wrong fails
silently inside `_spatial` with an unknown-field error.

## Predicate semantics

| Type | What it means | Use case |
|------|---------------|----------|
| `INTERSECTS` | Geometries share ANY portion of space | "Roads crossing this region" |
| `WITHIN`     | The joined geometry sits ENTIRELY inside the parent | "Buildings inside this city boundary" |
| `CONTAINS`   | The parent ENTIRELY contains the joined geometry | "Zones that contain this point" |
| `DISJOINT`   | No shared space | "Development zones NOT overlapping protected areas" |
| `DWITHIN`    | Within `buffer` meters | "Hospitals within 10km" (requires `buffer:`) |

`WITHIN` and `CONTAINS` are inverses — pick the one whose
"parent" is what your top-level select returns.

## Filter / sort / paginate

Two distinct knobs:

- **Pre-spatial** — applied to the joined object **before** the
  spatial join executes:
  `filter`, `order_by`, `limit`, `offset`, `distinct_on`.
- **Post-spatial** — applied to the result of the spatial join:
  `nested_order_by`, `nested_limit`, `nested_offset`.

```graphql
# Catalog prefix in this example: `app` (so the customer TYPE
# is `app_customers`; the module-path query field is just
# `customers`). Module path elided for brevity — wrap in
# `query { <module> { ... } }` as your discovery results show.
stores {                              # unprefixed query field
  id name location
  _spatial(field: "location", type: DWITHIN, buffer: 5000) {
    app_customers(                    # prefixed type name (flat)
      field: "address_location"
      filter: { active: { eq: true } }              # pre-spatial
      nested_order_by: [{ field: "name", direction: ASC }]
      nested_limit: 10                              # 10 nearest active customers per store
    ) {
      id name
    }
  }
}
```

**Distance is NOT a built-in field on `_spatial` results.** The
runtime does not generate a `_distance` / `distance_meters`
field — only `DWITHIN buffer:` filters by max radius, with no
distance value coming back. To rank candidates by proximity:

- Best: ask `schema-type_fields(type_name: "<type>")` to check
  whether the catalog author defined a derived distance field
  (often via `@sql`). If yes — use that name in
  `nested_order_by`.
- Otherwise: fetch candidates via `DWITHIN` with a sensible
  buffer + `nested_limit`, then post-process client-side.

Per-geometry size measurements ARE built in via
`_<geomfield>_measurement(type: <GeometryMeasurementTypes!>)`
on every geometry-bearing object. Enum values:
`Area`, `AreaSpheroid`, `Length`, `LengthSpheroid`,
`Perimeter`, `PerimeterSpheroid`. These are measurements of a
single geometry — they do NOT compute distance between two.

## inner vs left join

Default is LEFT JOIN — parents with no spatial match still appear
with an empty nested array. Pass `inner: true` on the joined
object to drop parents with zero matches.

```graphql
# Only stores that actually have a customer within 5km:
stores {                                              # top-level: unprefixed
  _spatial(field: "location", type: DWITHIN, buffer: 5000) {
    app_customers(field: "address_location", inner: true) { id }
  }
}

# Inverse — find delivery zones with NO active orders (coverage gap):
delivery_zones {
  _spatial(field: "boundary", type: CONTAINS) {
    app_orders(field: "delivery_location",
               filter: { status: { eq: "active" } }) { id }
  }
}
# Filter client-side for parents where the array is empty.
```

## In aggregations

`_spatial` works inside aggregation queries too — single-row and
bucket variants.

### Count matches per parent

```graphql
cities {                                          # top-level: unprefixed
  id name boundary
  _spatial(field: "boundary", type: CONTAINS) {
    app_buildings_aggregation(field: "footprint") { _rows_count }
  }
}
```

### Bucket inside spatial

```graphql
districts {
  id name boundary
  _spatial(field: "boundary", type: CONTAINS) {
    app_businesses_bucket_aggregation(field: "location") {
      key { business_type }
      aggregations { _rows_count revenue { sum avg } }
    }
  }
}
```

### Spatial inside bucket key (group BY a spatial relation)

```graphql
orders_bucket_aggregation {                       # top-level: unprefixed
  key {
    status
    delivery_zone: _spatial(field: "delivery_location", type: WITHIN) {
      app_zones(field: "boundary") { zone_id zone_name }
    }
  }
  aggregations { _rows_count total { sum avg } delivery_time { avg } }
}
```

This groups orders by `(status, delivery_zone)` — read as
"average delivery time per status per zone".

## With dynamic joins

Combine `_spatial` with `_join` to follow attribute relations
after a spatial match. `_join` uses the same flat-prefixed-name
rule:

```graphql
stores {                                          # top-level: unprefixed
  location
  _spatial(field: "location", type: DWITHIN, buffer: 5000) {
    app_customers(field: "address_location") {    # flat prefixed in _spatial
      id
      _join(fields: ["id"]) {
        app_orders(fields: ["customer_id"]) {     # flat prefixed in _join
          id
          total
        }
      }
    }
  }
}
```

## Common shapes (cheat sheet)

| Goal | Predicate | Notes |
|------|-----------|-------|
| Things inside a region | `WITHIN` (region → things) or `CONTAINS` (things → region) | Pick by which side is the top-level select |
| Nearest N | `DWITHIN buffer: <max>` + `nested_limit: N` | No built-in distance field — sort client-side, or use a derived `@sql` field if the catalog defined one |
| Coverage gap | `INTERSECTS` LEFT JOIN; filter client-side for empty arrays | Use `inner: false` (default) |
| Spatial group-by | `_spatial` inside `_bucket_aggregation.key` | Returns a grouping dimension |
| Roads crossing | `INTERSECTS` | Linear vs polygon overlap |
| Strict containment | `WITHIN` (one direction) or `CONTAINS` (the other) | "Boundary inside boundary" |

## Critical rules

- **No module nesting inside `_spatial { ... }`.** Reference data
  objects by their **prefixed type name** (same form
  `schema-type_fields(type_name: ...)` expects, same form returned
  by `discovery-search_module_data_objects` as `name`). Module
  nesting (`module { submodule { object { ... } } }`) belongs to
  the top-level query path; inside `_spatial`, `_join`, and
  `h3 > data` blocks the namespace is flat.
- `buffer` is **only** meaningful for `DWITHIN`. Setting it for
  any other type is silently ignored.
- The TWO `field:` arguments name DIFFERENT columns
  (parent's geometry; joined object's geometry) — name mismatch
  is the most common cause of empty results.
- Run `schema-type_fields(type_name: "<type>")` first to confirm
  geometry column names; never guess `geom` / `geometry` /
  `location`.
- For cross-source `_spatial`, see
  [extensions docs](https://hugr-lab.github.io/docs/engine-configuration/extension#using-spatial-queries-_spatial-across-sources)
  — the extension must be enabled on the data source.
- Always paginate the joined object (`limit`, `nested_limit`,
  or aggregate to `_rows_count`) — unbounded spatial joins on
  multi-million-row datasets stall the engine.
- Confirm SRID consistency — mixing 4326 and 3857 geometries
  yields wrong results without an explicit transform.

## See also

- `h3-spatial.md` — H3 hexagonal grid aggregation +
  `distribution_by` (population/area density).
- `query-patterns.md` — `_join` (attribute-based dynamic join,
  the non-spatial sibling of `_spatial`).
- `aggregations.md` — `_aggregation` / `_bucket_aggregation`
  basics.
