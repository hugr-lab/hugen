# Advanced Features Reference

## Vector Similarity Search

Hugr supports vector similarity search for PostgreSQL (pgvector) and DuckDB (vss extension).

### Basic Similarity Search

```graphql
query {
  documents(
    similarity: {
      name: "embedding"        # Vector field name
      vector: [0.1, 0.2, ...]  # Query vector
      distance: Cosine          # Cosine, L2, or Inner
      limit: 10                 # Max results
    }
  ) {
    id title content
  }
}
```

### Distance Metrics
- `Cosine` — best for text embeddings, normalized vectors (range [-1, 1])
- `L2` — best for image embeddings, unnormalized vectors
- `Inner` — best for collaborative filtering, recommendations

### Calculated Distance Fields
Auto-generated `_<field>_distance` field for each vector field:
```graphql
query {
  documents(
    similarity: { name: "embedding", vector: [...], distance: Cosine, limit: 10 }
  ) {
    id title
    _embedding_distance(vector: [...], distance: Cosine)
  }
}
```

### Combining with Filters
Filters apply BEFORE similarity search:
```graphql
query {
  products(
    similarity: { name: "embedding", vector: [...], distance: Cosine, limit: 5 }
    filter: { category: { eq: "electronics" }, in_stock: { eq: true } }
  ) {
    id name
    _embedding_distance(vector: [...], distance: Cosine)
  }
}
```

### Text-to-Vector with @embeddings
If configured, use `_distance_to_query` for text-based search:
```graphql
query {
  articles(
    similarity: { name: "content_embedding", vector: [...], distance: Cosine, limit: 10 }
  ) {
    id title
    relevance: _distance_to_query(query: "machine learning tutorials")
  }
}
```

---

## Generated Fields & Transformations

### DateTime / Timestamp Transformations

**Bucketing** — `bucket` argument: `minute`, `hour`, `day`, `week`, `month`, `quarter`, `year`
**Custom intervals** — `bucket_interval: "15 minutes"`
**Extract parts** — `_<field>_part(extract: year)`: epoch, minute, hour, day, doy, dow, iso_dow, week, month, year, iso_year, quarter
**Extract divide** — `_<field>_part(extract: hour, extract_divide: 6)` — divides extracted value for custom grouping (e.g., 6-hour blocks)

### JSON Transformations

**struct argument** — extract typed fields from JSON (field remains JSON, trimmed to structure):
```graphql
query {
  events {
    id
    metadata(struct: {
      user_id: "int"
      action: "string"
      details: { ip_address: "string" }
    })
  }
}
```

Type mappings: `string`, `int`, `bigint`, `float`, `boolean`, `time`, `json`, `h3String`
Arrays: `tags: ["string"]`, `prices: ["float"]`

**JSON path in aggregations** — dot notation:
```graphql
events_aggregation {
  metadata {
    count(path: "user_id")
    sum(path: "details.amount")
    avg(path: "details.score")
    list(path: "tags", distinct: true)
    string_agg(path: "name", separator: ", ")
    bool_and(path: "active")
    any(path: "status")
  }
}
```

### Geometry Transformations

**Geometry transforms** — `transforms` argument:
- `Transform` — reproject (requires `from` and `to` SRID)
- `Centroid` — calculate center
- `Buffer` — create buffer zone (`buffer` parameter in meters)
- `Simplify` — simplify geometry (`simplify_factor`)
- `SimplifyTopology`, `StartPoint`, `EndPoint`, `Reverse`, `FlipCoordinates`, `ConvexHull`, `Envelope`

```graphql
query {
  parcels {
    id
    center: boundary(transforms: [Centroid])
    buffer_zone: boundary(transforms: [Buffer], buffer: 100.0)
    simplified: boundary(transforms: [Simplify], simplify_factor: 0.001)
  }
}
```

**Geometry measurements** — `_<field>_measurement(type: ...)`:
- `Area`, `AreaSpheroid` (m²)
- `Length`, `LengthSpheroid` (m)
- `Perimeter`, `PerimeterSpheroid` (m)

```graphql
query {
  parcels {
    id
    area_m2: _boundary_measurement(type: AreaSpheroid)
    perimeter_m: _boundary_measurement(type: PerimeterSpheroid)
  }
}
```

**Geometry aggregation functions**: `count`, `union`, `intersection`, `extent`, `any`, `last`, `list(distinct: true)`

---

## @unnest Directive in Bucket Aggregations

Flatten subquery results for aggregation (like SQL JOIN):
```graphql
orders_bucket_aggregation {
  key {
    customer { country }
    order_details @unnest {
      product { category { name } }
    }
  }
  aggregations {
    _rows_count
    total { sum }
    order_details { quantity { sum } unit_price { avg } }
  }
}
```

**Warning**: `@unnest` multiplies rows like SQL JOIN. Use carefully!

---

## Mutations

### Insert
```graphql
mutation {
  insert_customers(data: {
    name: "John Doe"
    email: "john@example.com"
    # Nested one-to-many
    addresses: [
      { street: "123 Main St", city: "New York", type: "billing" }
    ]
  }) {
    id name email
  }
}
```

### Update
```graphql
mutation {
  update_customers(
    filter: { id: { eq: 123 } }
    data: { email: "new@example.com" }
  ) {
    affected_rows success message
  }
}
```

### Delete
```graphql
mutation {
  delete_orders(
    filter: { status: { in: ["cancelled", "expired"] } }
  ) {
    affected_rows
  }
}
```

### Transactions
All mutations in a single request execute atomically. If any fails, all are rolled back.

### Soft Delete
Tables with `@soft_delete` — records marked with `deleted_at` instead of physical deletion.
Query soft-deleted: `@with_deleted` directive.
Hard delete: `hard_delete: true` argument.

### Mutation Functions
```graphql
mutation {
  mutation_function {
    module_name {
      my_mutation(arg: "value") { success affected_rows }
    }
  }
}
```

### Auto-Generated Values
Fields with `@default` directive: sequences, `insert_exp: "NOW()"`, `update_exp: "NOW()"`, static values.

### Semantic Search Integration
Use `summary` parameter in insert/update to generate embeddings:
```graphql
mutation {
  insert_documents(
    data: { title: "...", content: "..." }
    summary: "Concise summary for embedding generation"
  ) { id }
}
```

---

## Cube Queries (@cube)

Tables with `@cube` — fields with `@measurement` are aggregated, others become dimensions.

### measurement_func values
- Numeric: `SUM`, `AVG`, `MIN`, `MAX`, `ANY`
- Boolean: `AND`, `OR`, `ANY`
- DateTime, Date, Timestamp: `MIN`, `MAX`, `ANY`

```graphql
query {
  sales {
    sale_date(bucket: month)
    region
    total_revenue: total_amount(measurement_func: SUM)
    avg_price: unit_price(measurement_func: AVG)
  }
}
```

### Double Aggregation
When using `measurement_func` in `_aggregation`, cube pre-aggregates first, then aggregation query runs on top:
```graphql
sales_aggregation {
  total_amount(measurement_func: SUM) {
    sum  # Sum of sums
    avg  # Average of sums
  }
}
```

### Measurement as Dimension
Query `@measurement` field WITHOUT `measurement_func` → it becomes a dimension (added to GROUP BY).

---

## Hypertable Queries (@hypertable)

Leverages TimescaleDB. Must have `@timescale_key` on timestamp field.

```graphql
query {
  sensor_readings_bucket_aggregation {
    key {
      hour: timestamp(bucket: hour)
      sensor_id
    }
    aggregations {
      _rows_count
      temperature { avg min max }
    }
  }
}
```

Can combine `@cube` + `@hypertable` for time-series analytical data.

---

## Time Travel with @at (DuckLake / Iceberg only)

The `@at` directive enables querying data as it existed at a specific snapshot version or timestamp.
Only works on DuckLake and Iceberg data sources — using it on other sources produces a compilation error.

### Syntax
```graphql
# By snapshot version
trips_aggregation @at(version: 5) { _rows_count }

# By timestamp (RFC 3339)
trips_aggregation @at(timestamp: "2026-01-15T10:30:00Z") { _rows_count }
```

**Placement**: `@at` goes AFTER arguments — `field(args) @at(version: N) { ... }`

### Works with
- `select`, `aggregation`, `bucket_aggregation`, `by_pk` queries
- Relations — resolved at the specified version
- All standard arguments (filter, order_by, limit, etc.)

### Does NOT work with
- Mutations (insert, update, delete) — error
- Non-time-travel data sources (DuckDB, PostgreSQL, etc.) — compilation error

### Comparing snapshots — ONE query with aliases
```graphql
query TimeTravel {
  taxi {
    # Current data
    current: trips_aggregation { _rows_count fare_amount { avg } }

    # Historical data at snapshot 5
    old: trips_aggregation @at(version: 5) { _rows_count fare_amount { avg } }

    # Bucket aggregation with time travel + relation
    old_by_zone: trips_bucket_aggregation(
      order_by: [{field: "aggregations._rows_count", direction: DESC}]
      limit: 5
    ) @at(version: 5) {
      key { pickup_zone { Zone Borough } }
      aggregations { _rows_count fare_amount { avg } }
    }
  }
}
```

### DuckLake management — snapshots and info
```graphql
# View snapshot history
query { core { ducklake {
  snapshots(args: { name: "my_lake" }) { snapshot_id snapshot_time changes }
} } }

# Get current snapshot and catalog info
query { function { core { ducklake {
  current_snapshot(name: "my_lake")
  info(name: "my_lake") { snapshot_count current_snapshot table_count data_path }
} } } }
```

---

## JQ Transformations

### Inline jq() query
Results are in `extensions.jq`:
```graphql
query {
  jq(query: ".users | map({id, name})", include_origin: false) {
    users { id name email }
  }
}
```

### Aliases for named transforms
```graphql
query {
  userCount: jq(query: ".users | length") { users { id } }
  topSpenders: jq(query: ".users | sort_by(-.total) | .[0:5]") { users { id total } }
}
```

### Hierarchical chaining
```graphql
query {
  jq(query: ".result | group_by(.category)") {
    result: jq(query: ".products | map({id, name, category, price})") {
      products { id name category price description }
    }
  }
}
```

### queryHugr() function (JQ-only)
Execute nested GraphQL queries from within JQ expressions — for data enrichment:
```jq
.customers | map(
  . + {
    recent_orders: queryHugr(
      "query($cid: Int!) { orders(filter: {customer_id: {eq: $cid}}, limit: 5) { id total } }",
      {cid: .id}
    ).data.orders
  }
)
```
**Warning**: queryHugr() in map() = N+1 queries. Use GraphQL relations when possible.

### Using Variables in JQ
GraphQL variables accessible with `$var_name`:
```graphql
jq(query: ".products | map(select(.price > $minPrice))")
```

### _jq Variable Input Transformation
Dynamically compute variables before query execution:
```json
{
  "variables": {
    "_jq": "{ from: (utcTime | roundTime(\"day\") | timeAdd(\"-7d\")), to: (utcTime | roundTime(\"day\")) }",
    "sensorId": "sensor-42"
  }
}
```
Available functions: `utcTime`, `localTime`, `roundTime`, `timeAdd`, `datePart`, `unixTime`, `authInfo`.

### /jq-query REST endpoint
- Receives full GraphQL response (data, errors, extensions)
- Response is direct JQ result (no GraphQL envelope)
- Caching headers: `X-Hugr-Cache: 5m`, `X-Hugr-Cache-Key: dashboard:stats`, `X-Hugr-Cache-Tags: analytics`, `X-Hugr-Cache-Invalidate: true`

### @cache directive in queryHugr()
```jq
queryHugr("{ stats: orders_bucket_aggregation @cache { key { status } aggregations { _rows_count } } }").data.stats
```

### Best Practice
Filter in GraphQL (not JQ) for performance. Use JQ for reshaping, not filtering.

---

## Spatial Queries (_spatial)

### Relationship Types
- `INTERSECTS` — geometries share any space
- `WITHIN` — geometry completely inside reference
- `CONTAINS` — reference completely contains target
- `DISJOINT` — no shared space
- `DWITHIN` — within distance (`buffer` in meters)

```graphql
stores {
  id name location
  _spatial(field: "location", type: DWITHIN, buffer: 5000) {
    customers(field: "address_location") { id name }
    customers_aggregation(field: "address_location") { _rows_count }
  }
}
```

### inner: true — INNER JOIN (exclude parents without spatial matches)
```graphql
_spatial(field: "location", type: DWITHIN, buffer: 5000) {
  customers(field: "address_location", inner: true) { id }
}
```

### _spatial in bucket aggregation keys
Group by spatially related data:
```graphql
orders_bucket_aggregation {
  key {
    status
    delivery_zone: _spatial(field: "delivery_location", type: WITHIN) {
      zones(field: "boundary") { zone_id zone_name }
    }
  }
  aggregations { _rows_count total { sum } }
}
```

### Multi-level spatial queries
Nested _spatial for hierarchical geographic analysis.

---

## H3 Hexagonal Clustering

```graphql
query {
  h3(resolution: 7) {
    cell resolution
    data {
      buildings: buildings_aggregation(field: "location", inner: true) {
        _rows_count
        area { sum }
      }
    }
  }
}
```

Resolution guidelines: 5-7 (city), 8-9 (neighborhood), 10-12 (street).

### Key arguments on H3 data aggregations
- `field` (required) — geometry field for spatial aggregation
- `inner: true` — only cells with data (skip empty hexagons)
- `divide_values: false` — keep original values (default true distributes proportionally across cells)
- `filter` — pre-filter data before spatial aggregation

### distribution_by — distribute values proportionally across H3 cells
Formula: `value = numerator * (denominator / denominator_total)`

```graphql
h3(resolution: 8) {
  data { ... }
  pop_distributed: distribution_by(
    numerator: "data.population.population.sum"
    denominator: "data.buildings.area.sum"
  ) { value ratio numerator denominator denominator_total }
}
```

### distribution_by_bucket — distribute across buckets within each cell

### Complete example: Population estimation via residential buildings

This pattern distributes census population across H3 cells proportionally to residential building area, using cross-source joins:

```graphql
query deOSMByH3 {
  h3(resolution: 6) @stats {
    cell
    resolution
    data {
      # 1. Spatial aggregation: admin boundaries (Landkreise) intersecting this H3 cell
      #    divide_values:false → keep full Landkreis population (don't split by overlap)
      #    inner:true → skip cells with no admin boundary
      lk: osm_bw_osm_administrative_boundaries_aggregation(
        field: "geom"
        filter: { admin_level: { eq: 6 } }
        divide_values: false
        inner: true
      ) {
        # 2. Cross-source _join: OSM boundaries → Zensus population by admin code
        pop: _join(fields: ["de_code"]) {
          zensus_population(fields: ["db_RS"]) {
            EWZ { sum }
          }
        }
      }

      # 3. Spatial aggregation: residential buildings in this H3 cell
      #    Default divide_values:true → area distributed proportionally
      houses: osm_bw_osm_buildings_aggregation(
        field: "geom"
        filter: { building_class: { eq: "residential" } }
      ) {
        _rows_count
        area_sqm { sum }
      }
    }

    # 4. Distribute population by residential building area
    #    pop = census_pop * (cell_housing_area / total_housing_area)
    pop: distribution_by(
      numerator: "data.lk.pop.zensus_population.EWZ.sum"
      denominator: "data.houses.area_sqm.sum"
    ) {
      value             # distributed population for this cell
      ratio             # cell's share of total housing area
      numerator         # source census population
      denominator       # housing area in this cell
      denominator_total # total housing area across all cells
    }
  }
}
```

Features used: H3 grid, @stats, cross-source spatial aggregation (OSM DuckDB), _join inside H3 (OSM→Zensus), divide_values control, distribution_by for proportional estimation.
