# Filter Guide

## Scalar Operators

Operators depend on the field type (check with `schema-type_fields`):

| Operator | Types | Description |
|----------|-------|-------------|
| `eq` | all scalars | Equal to |
| `gt`, `gte` | numeric, temporal, Interval | Greater than (or equal) |
| `lt`, `lte` | numeric, temporal, Interval | Less than (or equal) |
| `in` | String, numeric, DateTime, Date, Time, Timestamp | In list |
| `is_null` | all | Is null check |
| `like` | String | SQL LIKE pattern (`%` wildcard) |
| `ilike` | String | Case-insensitive LIKE |
| `regex` | String | Regular expression |
| `has` | JSON | Has key |
| `has_all` | JSON | Has all keys |
| `contains` | JSON, Geometry, List, Range | Contains value/geometry/list/range |
| `intersects` | Geometry, List, Range | Intersects with |
| `includes` | Range | Range includes another range |
| `eq` (List) | List types | List contains value |

Boolean type supports only: `eq`, `is_null`.

## Logical Operators

`_and`, `_or` take arrays. `_not` takes a single filter object. Multiple conditions at the same level are implicit AND.

```graphql
# Explicit AND + OR
filter: {
  _and: [
    {status: {eq: "active"}}
    {_or: [
      {age: {gte: 18}}
      {role: {eq: "admin"}}
    ]}
  ]
}
```

### _not — Negation

`_not` wraps a **filter object**, not a field operator. There is **NO `neq` operator**.

```graphql
# "Not equal" — correct way
filter: { _not: { status: { eq: "cancelled" } } }

# "Not in list"
filter: { _not: { status: { in: ["cancelled", "expired"] } } }

# "Not like pattern"
filter: { _not: { description: { ilike: "%test%" } } }

# NOT (A AND B) — multiple fields inside _not
filter: { _not: { status: { eq: "active" }, role: { eq: "admin" } } }

# _not inside _and — "active diabetes patients still sick"
filter: {
  _and: [
    { description: { ilike: "%diabetes%" } }
    { _not: { stop: { is_null: false } } }
  ]
}
```

### ❌ Common Mistakes

```graphql
# WRONG: neq does not exist
filter: { status: { neq: "cancelled" } }
# CORRECT:
filter: { _not: { status: { eq: "cancelled" } } }

# WRONG: ne does not exist
filter: { id: { ne: 123 } }
# CORRECT:
filter: { _not: { id: { eq: 123 } } }

# WRONG: not_in does not exist
filter: { status: { not_in: ["a", "b"] } }
# CORRECT:
filter: { _not: { status: { in: ["a", "b"] } } }
```

## Relation Filters

Filter by related objects (up to 4 levels deep):

```graphql
filter: {
  category: {description: {ilike: "%premium%"}}
  customers: {any_of: {country: {eq: "US"}}}
}
```

For lists and one-to-many/many-to-many relations use: `any_of`, `all_of`, `none_of`:

```graphql
orders(filter: {
  items: {any_of: {product: {category: {eq: "electronics"}}}}
}) {
  id total
  items { product { name category } }
}
```

## Tips

- Check the filter input type fields using `schema-type_fields` to see available operators.
- Filter by relations to limit data early.
- Combine filters to minimize query size and processing time.
- Use `in` instead of multiple `_or` with `eq` for better performance.
- Multiple filters at the same level are implicit AND (no `_and` needed).
- Relation filters only work for `@field_references` / `@references` relations, NOT `@join` or `@table_function_call_join`.
- For `@join` fields, use `inner: true` instead.

## Geometry Filter Input Formats

- **WKT**: `"POINT(1 2)"`, `"POLYGON((0 0, 0 1, 1 1, 1 0, 0 0))"`
- **GeoJSON**: `{ type: "Point", coordinates: [1, 2] }`

```graphql
filter: { boundary: { intersects: "POLYGON((0 0, 0 100, 100 100, 100 0, 0 0))" } }
filter: { boundary: { contains: { type: "Point", coordinates: [-73.98, 40.75] } } }
```

## JSON Filter Examples

```graphql
filter: {
  metadata: {
    contains: { "user_id": 123 }   # PostgreSQL @> operator
    has: "transaction_id"           # has key
    has_all: ["user_id", "tid"]     # has all keys
  }
}
```

## Array Filter Examples

```graphql
filter: {
  tags: {
    contains: ["featured", "sale"]    # array has ALL specified values
    intersects: ["featured", "new"]   # array has ANY of values
    eq: ["sale", "clearance"]         # exact array match
  }
}
```
