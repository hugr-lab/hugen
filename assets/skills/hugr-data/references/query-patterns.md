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

## Spatial joins (`_spatial`) and H3 aggregation

Split out to dedicated references — both follow the same flat-
prefixed naming rule as `_join` above (module nesting outside,
prefixed type name inside):

- **`_spatial`** (`INTERSECTS / WITHIN / CONTAINS / DISJOINT /
  DWITHIN`, inner-vs-left, spatial inside aggregation keys,
  nearest-N) — see `spatial-queries.md`.
- **`h3(resolution:)`** with `data { ... }`, `divide_values`,
  `inner`, `distribution_by`, `distribution_by_bucket`, and
  cross-source patterns via `_join` inside `data` — see
  `h3-spatial.md`.

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
