# Query Patterns

## Basic Select

```graphql
query {
  module_name {
    table_name(filter: {field: {eq: "value"}}, order_by: [{field: "name", direction: ASC}], limit: 10) {
      field1
      field2
    }
  }
}
```

## Select One (by PK)

```graphql
query {
  module {
    table_by_pk(id: 1) {
      id name
    }
  }
}
```

## Reference Traversal

```graphql
query {
  module {
    orders {
      id total
      customer { name category }
      items(nested_limit: 5) {
        product { name price }
      }
    }
  }
}
```

## Relations with Aggregation

```graphql
query {
  module {
    customers {
      id name
      orders_aggregation {
        _rows_count
        total: amount { sum avg }
      }
      orders_bucket_aggregation {
        key { status }
        aggregations { _rows_count total: amount { sum avg } }
      }
    }
  }
}
```

## Filter by Relations

```graphql
query {
  module {
    orders(filter: {
      customer: {category: {eq: "premium"}}
      items: {any_of: {product: {category: {eq: "electronics"}}}}
    }) {
      id total
      customer { name }
    }
  }
}
```

## Nested Sorting & Pagination

```graphql
query {
  module {
    customers {
      id name
      orders(
        filter: {status: {eq: "active"}}
        nested_order_by: [{field: "total", direction: DESC}]
        nested_limit: 3
      ) {
        id total status
      }
    }
  }
}
```

## Function Calls

Functions are called via the top-level `function` field:

```graphql
query {
  function {
    module {
      my_function(arg1: "value") {
        result_field
      }
    }
  }
}
```

## Mutation Functions

```graphql
mutation {
  mutation_function {
    module {
      my_mutation(input: "data") {
        success
        affected_rows
      }
    }
  }
}
```

## Bucket Aggregation with Sorting

```graphql
query {
  module {
    orders_bucket_aggregation(
      order_by: [
        {field: "aggregations.total.sum", direction: DESC}
        {field: "key.status", direction: ASC}
      ]
      limit: 10
    ) {
      key { status }
      aggregations {
        _rows_count
        total: amount { sum avg }
      }
      filtered: aggregations(filter: {category: {eq: "premium"}}) {
        _rows_count
        total: amount { sum avg }
      }
    }
  }
}
```

## JQ Transform

Apply jq transformation to query results at the root level:

```graphql
query {
  jq(query: "{ module { table { id name } } }", jq: ".module.table | map(.name)")
}
```

## Query-Time Join (`_join`)

Join data from different tables at query time by matching field values. The `_join` field is available on every table/view. Inside `_join`, table names use the **catalog prefix** (not the module name).

```graphql
query {
  module {
    products(filter: { id: { eq: 1 } }) {
      id name category_id
      _join(fields: ["category_id"]) {
        prefix_categories(fields: ["id"]) {
          id name
        }
        prefix_categories_aggregation(fields: ["id"]) {
          _rows_count
        }
      }
    }
  }
}
```

Arguments on `_join` subfields:
- `fields: [String!]!` — target table fields to match against source `_join(fields: ...)`
- `filter`, `order_by`, `limit`, `offset`, `distinct_on` — applied **before** the join
- `nested_order_by`, `nested_limit`, `nested_offset` — applied **after** the join
- `inner: true` — use INNER JOIN instead of LEFT JOIN

## Spatial Join (`_spatial`)

Join data using geometry intersection. The `_spatial` field is available on tables/views with geometry columns.

```graphql
query {
  module {
    locations {
      id name point
      _spatial(field: "point", type: INTERSECTS) {
        prefix_areas(field: "geom") { id name }
        prefix_areas_aggregation(field: "geom") { _rows_count }
      }
    }
  }
}
```

Arguments on `_spatial`:
- `field: String!` — source geometry field name
- `type: GeometrySpatialQueryType!` — `INTERSECTS`, `WITHIN`, `CONTAINS`, `DISJOINT`, `DWITHIN`
- `buffer: Int` — buffer distance in meters (required for `DWITHIN`)

## H3 Spatial Aggregation

Basic H3 grid:
```graphql
query {
  h3(resolution: 4) {
    cell resolution geom
    data {
      prefix_locations_aggregation(field: "point") { _rows_count }
    }
  }
}
```

### H3 with cross-source _join and distribution_by

Population estimation: distribute census data proportionally by residential building area.
```graphql
h3(resolution: 6) {
  cell resolution
  data {
    # Admin boundaries → _join to census (cross-source)
    lk: prefix_boundaries_aggregation(
      field: "geom"
      filter: { admin_level: { eq: 6 } }
      divide_values: false    # keep original census totals
      inner: true             # skip empty cells
    ) {
      pop: _join(fields: ["code"]) {
        prefix_census(fields: ["admin_code"]) {
          population { sum }
        }
      }
    }
    # Residential buildings as denominator
    houses: prefix_buildings_aggregation(
      field: "geom"
      filter: { building_class: { eq: "residential" } }
    ) {
      _rows_count
      area_sqm { sum }
    }
  }
  # Distribute population by housing area
  pop: distribution_by(
    numerator: "data.lk.pop.prefix_census.population.sum"
    denominator: "data.houses.area_sqm.sum"
  ) { value ratio numerator denominator denominator_total }
}
```

Key H3 arguments:
- `field` — geometry field for spatial agg
- `inner: true` — only cells with data
- `divide_values: false` — don't split values by cell overlap (keep originals)
- `distribution_by` paths reference the `data` structure above

## Cube Tables (@cube)

Tables with `@cube` have `@measurement` fields with `measurement_func` argument (SUM, AVG, MIN, MAX, ANY):

```graphql
query {
  SalesCube(filter: {region: {eq: "US"}}) {
    region product
    revenue(measurement_func: SUM)
    quantity(measurement_func: AVG)
  }
}
```

## Distinct On

```graphql
query {
  module {
    orders(
      distinct_on: ["customer_id"]
      order_by: [{field: "customer_id", direction: ASC}, {field: "created_at", direction: DESC}]
    ) {
      customer_id created_at total
    }
  }
}
```

Note: first `order_by` field must be one of the `distinct_on` fields.
