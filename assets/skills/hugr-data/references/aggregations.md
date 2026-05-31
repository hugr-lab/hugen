# Aggregations

## Single-Row Aggregation

```graphql
query {
  module {
    table_aggregation(filter: {...}) {
      _rows_count
      price { sum avg min max }
    }
  }
}
```

## Bucket Aggregation (GROUP BY)

```graphql
query {
  module {
    table_bucket_aggregation {
      key { status }
      aggregations {
        _rows_count
        total: amount { sum avg }
      }
    }
  }
}
```

Group by nested fields:

```graphql
table_bucket_aggregation {
  key { customer { category { name } } }
  aggregations { _rows_count }
}
```

## Filtered Aggregation

Apply filter inside aggregation (acts as SQL `FILTER (WHERE ...)`). Use aliases for multiple filtered aggregations:

```graphql
query {
  module {
    orders_bucket_aggregation {
      key { status }
      all: aggregations { _rows_count total: amount { sum avg } }
      in_stock: aggregations(filter: {stock_quantity: {gt: 0}}) { _rows_count }
      premium: aggregations(filter: {category: {eq: "premium"}}) {
        _rows_count
        total: amount { sum avg }
      }
    }
  }
}
```

## Sorting Bucket Aggregations

Use dot-notation paths to sort by keys or aggregation values:

```graphql
orders_bucket_aggregation(
  order_by: [
    {field: "aggregations.total.sum", direction: DESC}
    {field: "key.status", direction: ASC}
  ]
  limit: 10
) {
  key { status }
  aggregations { total: amount { sum avg } }
}
```

Sorting by filtered aggregation alias:

```graphql
order_by: [{field: "filtered.total.sum", direction: DESC}]
```

## Aggregation Over Relations (Subquery Aggregation)

Aggregate related data for each parent record. Works with references, joins, and function calls:

```graphql
query {
  module {
    customers {
      id name
      orders_aggregation {
        _rows_count
        total: amount { sum avg }
      }
      completed: orders_aggregation(filter: {status: {eq: "completed"}}) {
        _rows_count
      }
      orders_bucket_aggregation {
        key { status }
        aggregations { _rows_count total: amount { sum avg } }
      }
    }
  }
}
```

## Sub-Aggregation (Aggregation of Aggregations)

Sub-aggregation applies aggregation functions to already-aggregated results:

```graphql
query {
  module {
    categories_aggregation {
      _rows_count
      products_aggregation {
        _rows_count { sum }
        price { avg { avg min max } }
      }
    }
  }
}
```

Here `products_aggregation` is a sub-aggregation: `_rows_count { sum }` computes the sum of per-category product counts, and `price { avg { avg } }` computes the average of per-category average prices.

## Time-Based Aggregations

Bucket by time periods:

```graphql
orders_bucket_aggregation {
  key { created_at(bucket: month) }
  aggregations { _rows_count total: amount { sum } }
}
```

Buckets: `minute`, `hour`, `day`, `week`, `month`, `quarter`, `year`.
Custom intervals: `created_at(bucket_interval: "15 minutes")`.
Extract parts: `_created_at_part(extract: year)`.

## Aggregations with _join and _spatial

Aggregation variants are available inside `_join` and `_spatial`:

```graphql
_join(fields: ["category_id"]) {
  prefix_products_aggregation(fields: ["category_id"]) {
    _rows_count
    price { sum avg }
  }
  prefix_products_bucket_aggregation(fields: ["category_id"]) {
    key { name }
    aggregations { _rows_count }
  }
}
```

## Count semantics — read this before introspecting aggregation types

Two different counts, and the difference matters:

- `_rows_count` — number of ROWS in the (filtered) group. Total records.
- `<field> { count }` — number of **DISTINCT (unique)** non-null values
  of that field. This is COUNT(DISTINCT field), NOT a row count.

So to count the unique entities behind many rows, put `count` on the
entity's id field — no `distinct_on`, no sub-query. One example covers
the common "how many roads, how long in total" shape over a parts table
(many parts per road):

```graphql
tf_road_parts_aggregation(field: "geom") {
  road_id { count }   # distinct roads
  len { sum }         # total length
  _rows_count         # total road-part rows (only if you also want it)
}
```

You do NOT need `schema-type_fields` on `BigIntAggregation` /
`FloatAggregation` / `_spatial_aggregation` to find this — every field
exposes the functions valid for its type (below), invoked as
`field { fn }`.

## Available Functions

- `_rows_count` — total rows in the group
- `<field> { count }` — DISTINCT (unique) non-null values of that field
- `sum`, `avg`, `min`, `max` — numeric
- `stddev`, `variance` — statistical
- `string_agg` — string concatenation
- `list` — collect into array
- `distinct` — distinct values
- `bool_and`, `bool_or` — boolean
- `any`, `last` — arbitrary value
- DateTime, Timestamp, Date — `min`, `max`, `count` only (NO sum/avg)
- JSON fields support `path` parameter for nested aggregation

## Functions by field type

Which aggregations are valid depends on the field's type — applying a
numeric function to a string is an error.

| Field type | Valid functions |
|------------|-----------------|
| Numeric | `sum`, `avg`, `min`, `max`, `count`, `stddev`, `variance` |
| String | `count`, `any`, `first`, `last`, `list` — **NO** `min` / `max` / `avg` / `sum` |
| DateTime / Timestamp / Date | `min`, `max`, `count` |
| Boolean | `bool_and`, `bool_or` |
| General (any type) | `any`, `last`, `count` |

Always available on the aggregation node itself: `_rows_count`.
