# Queries Deep Dive Reference

## Custom JQ Functions

Available in all JQ contexts: inline `jq()`, `/jq-query` endpoint, `_jq` variable input.

### Time Functions

| Function | Signature | Description |
|----------|-----------|-------------|
| `localTime` | `localTime` (no args) | Current local time (server TZ) |
| `utcTime` | `utcTime` (no args) | Current UTC time |
| `unixTime` | `<time> \| unixTime` | Convert to Unix timestamp; `null \| unixTime` = current |
| `roundTime` | `<time> \| roundTime(period)` or `roundTime(period; timezone)` | Truncate to period |
| `timeAdd` | `<time> \| timeAdd(duration)` or `timeAdd(duration; timezone)` | Add Go duration |
| `datePart` | `<time> \| datePart(part)` or `datePart(part; timezone)` | Extract component |

**roundTime periods**: `second`, `minute`, `hour`, `day`, `month`, `year`, `monday`..`saturday`, or Go duration `"15m"`, `"2h30m"`
**timeAdd durations**: Go format: `"1h"`, `"30m"`, `"-1h"` (negative to subtract)
**datePart parts**: `second`, `minute`, `hour`, `day`, `month`, `year`, `weekday` (1=Mon..7=Sun), `yearday`

```jq
# 7-day window ending at start of today
utcTime | roundTime("day") | timeAdd("-7d")

# Get hour of day
"2024-06-15T14:30:45Z" | datePart("hour")  # 14

# Chain: start of day + 9 hours
utcTime | roundTime("day") | timeAdd("9h")
```

### Authentication Function

`authInfo` — returns `{ userId, userName, role }` or `null` if unauthenticated.
```jq
if authInfo then "Hello, " + authInfo.userName else "Anonymous" end
```

---

## Filtering — Extended Operators

### Geometry Filters
```graphql
filter: {
  boundary: {
    intersects: "POLYGON((0 0, 0 100, 100 100, 100 0, 0 0))"  # WKT
    contains: { type: "Point", coordinates: [-73.98, 40.75] }    # GeoJSON
    is_null: false
  }
}
```
Geometry input: WKT strings or GeoJSON objects. Operators: `eq`, `intersects`, `contains`, `is_null`.

### JSON/JSONB Filters
```graphql
filter: {
  metadata: {
    contains: { "user_id": 123 }   # contains key-value (@>)
    has: "transaction_id"           # has key
    has_all: ["user_id", "tid"]     # has all keys
    eq: { "status": "active" }      # exact match
  }
}
```

### Array Filters
```graphql
filter: {
  tags: {
    contains: ["featured", "sale"]    # array contains ALL values
    intersects: ["featured", "new"]   # array has ANY of values
    eq: ["sale", "clearance"]         # exact array match
  }
}
```

### Range Filters
Operators: `contains`, `intersects`, `includes` for Range types.

### @filter_required
Schema-level directive forcing certain fields to be filtered:
```graphql
# This table requires timestamp filter — queries without it will fail
time_series_data(filter: { timestamp: { gte: "2024-01-01", lt: "2024-02-01" } }) { ... }
```

### Filter Variables
```graphql
query GetActive($filter: customers_filter!) {
  customers(filter: $filter) { id name }
}
```

---

## Sorting — Extended Patterns

### Sort by Related Fields (dot-path)
```graphql
orders(order_by: [{ field: "customer.country", direction: ASC }]) {
  id
  customer { country }  # MUST be selected
}
```

### Sort by Aggregated Relations
```graphql
customers(order_by: [{ field: "orders_aggregation.total.sum", direction: DESC }]) {
  id name
  orders_aggregation { total { sum } }  # MUST be selected
}
```

### Field Aliases in order_by
Use the **alias name**, not the original field name:
```graphql
products(order_by: [{ field: "category_name", direction: ASC }]) {
  category { category_name: name }
}
```

### Cursor-Based Pagination
Better than offset for large datasets:
```graphql
customers(filter: { id: { gt: $cursor } }, order_by: [{field: "id", direction: ASC}], limit: 20) { id name }
```

### Pre-Join vs Post-Join Arguments on Subqueries

Subquery fields (relations) support two sets of arguments:

**Pre-join** (applied to the related table BEFORE joining with parent):
- `filter` — filter rows in the related table
- `order_by` — sort rows in the related table
- `limit` / `offset` — limit rows from the related table
- `distinct_on` — deduplicate in the related table (global, not per-parent)

**Post-join** (applied AFTER joining, per parent record):
- `nested_order_by` — sort related records per parent
- `nested_limit` / `nested_offset` — paginate per parent
- `inner: true` — INNER JOIN (exclude parents without matches); also guarantees non-null sub-aggregation results

**Key distinction**: `distinct_on` is pre-join — it deduplicates across the entire related table, not per parent. For "one per group per parent" use `nested_limit: 1` with `nested_order_by` instead.

```graphql
customers(limit: 10) {
  id name

  # Post-join: top-3 biggest payments PER customer
  top_payments: general_payments(
    nested_order_by: [{field: "amount", direction: DESC}]
    nested_limit: 3
  ) { id amount }

  # Post-join: latest payment PER customer (acts like "distinct per parent")
  latest_payment: general_payments(
    nested_order_by: [{field: "date", direction: DESC}]
    nested_limit: 1
  ) { id date amount }

  # Pre-join: filter + sort the payments table first, then join
  active_payments: general_payments(
    filter: { status: { eq: "active" } }
    order_by: [{field: "date", direction: DESC}]
    limit: 100
  ) { id date }

  # inner: true on sub-aggregation — guarantees non-null results
  general_payments_aggregation(inner: true) {
    _rows_count
    amount { sum avg }
  }
}
```

---

## Relations — Extended Patterns

### inner: true — INNER JOIN
Default is LEFT JOIN. Use `inner: true` to exclude parents without matches:
```graphql
customers {
  id name
  orders(filter: { status: { eq: "pending" } }, inner: true) { id }
  # Only customers with pending orders are returned
}
```

### Predefined Joins (@join directive)
Custom join conditions defined in schema (e.g., spatial joins, complex SQL):
```graphql
customers {
  nearby_stores(inner: true) { id name }
}
```
**Note**: `@join` fields cannot be used in `filter` conditions. Use `inner: true` instead.

### @table_function_call_join
Join function results with data objects. Mapped args come from parent fields; unmapped args become query parameters:
```graphql
sensors {
  readings(from_time: "2024-01-01T00:00:00Z", to_time: "2024-01-31T23:59:59Z") {
    timestamp value
  }
}
```
**Limitation**: No filter/order_by/limit/aggregations support. Use **parameterized views** instead.

### Parameterized Views (@args)
Full query support (filter, order_by, limit, aggregation) with required arguments:
```graphql
sensors {
  readings(args: { sensor_id: 123, from_time: "...", to_time: "..." }
           filter: { value: { gt: 100 } }
           order_by: [{field: "timestamp", direction: DESC}]
           limit: 50) {
    timestamp value
  }
  readings_aggregation(args: { ... }) { _rows_count value { avg min max } }
}
```

### Self-Referential Relations
Hierarchical data (e.g., employees → manager, subordinates):
```graphql
employees { id name manager { name } subordinates { name } }
```

---

## Function Fields

### @function_call — Embed Functions as Fields
Maps function arguments to parent object fields. Called per-row.
```graphql
# Schema: shipping_cost field calls calculate_shipping_cost with args from order fields
orders { id shipping_cost }
```

### Partial Argument Mapping
Some args from fields, others from query:
```graphql
products { price_converted(to_currency: "EUR") }
```

### Table Function Fields
Functions returning arrays — support `filter`, `order_by`, `limit`:
```graphql
customers {
  recommendations(limit: 5, filter: { price: { lte: 100 } }) { id name }
}
```

### Cross-Source Function Fields
Call functions from different data sources (HTTP APIs, other DBs):
```graphql
orders { tracking_info { status estimated_delivery } }
```

### skip_null_arg: true
Function called even if mapped arg is NULL, but NULL not passed to SQL.

---

## GraphQL Extensions

### @stats Directive — Performance Monitoring
```graphql
query @stats {                       # query-level: total_time
  products @stats { id name }        # field-level: compile_time, exec_time, node_time, planning_time
}
```

Metrics:
- `compile_time` — query compilation
- `exec_time` — SQL execution
- `planning_time` — query planning
- `node_time` — total for the node
- `total_time` — overall (query-level only)

JQ-specific stats (when `@stats` on `jq()` query):
- `compiler_time`, `data_request_time`, `execution_time`, `serialization_time`, `runs`, `transformed`

Results in `extensions` field of response. Minimal overhead (~0.01ms per field).

### JQ Results Location
- Inline `jq()`: results in `extensions.<alias_or_jq>.jq`
- `/jq-query` endpoint: direct HTTP response body

---

## Access Control (RBAC) — Agent Behavior

Hugr uses role-based access control. Schemas are dynamically filtered by user role.

### How permissions work
- **Open by default** — if no permission entry exists for a type/field, it's accessible
- **Wildcards** — `type_name: "*"` or `field_name: "*"` apply broadly
- **Priority** — exact match > type+wildcard field > wildcard type+exact field > both wildcards > default (allowed)
- **hidden: true** — field excluded from response unless explicitly requested in query
- **disabled: true** — field/type completely inaccessible, returns error

### What the agent sees
- Discovery tools (`search_modules`, `search_module_data_objects`, `schema-type_fields`) return **only accessible** objects and fields
- If an object/field is `disabled`, it won't appear in discovery results at all
- If `hidden`, it appears in schema but not in query results unless explicitly selected
- Row-level filters are applied silently — the agent doesn't see them, but data is pre-filtered

### Common restricted scenarios
- **Read-only role** — all mutations disabled (`Mutation.*` disabled)
- **Aggregation-only access** — select queries disabled, only `_aggregation` and `_bucket_aggregation` available
- **Field-level restrictions** — sensitive fields (email, SSN, etc.) hidden or disabled
- **Row-level security** — user sees only records matching their `[$auth.user_id]`, `[$auth.tenant_id]`, etc.
- **Module-level restrictions** — entire modules invisible to certain roles

### Agent guidelines
1. **Never assume full access** — always introspect first, work with what's available
2. **If a query fails with permission error** — explain the restriction to the user, suggest alternatives
3. **If discovery returns empty or fewer results** — note that additional data may be restricted
4. **If mutations fail** — the role may be read-only
5. **Adapt query strategy** — if only aggregations are available, use them; don't try to force select queries
6. **Self-described sources without descriptions** (e.g. DuckLake with `self_defined: true`) — descriptions come from summarization, which may not have been run; field names alone are sufficient for querying

### Permission types for reference
| Permission target | `type_name` | `field_name` |
|---|---|---|
| All queries/mutations globally | `*` | `*` |
| All mutations | `Mutation` | `*` |
| Specific mutation | `Mutation` | `insert_articles` |
| Root-level query field | `Query` | `articles` |
| Module-level query | `_module_<module>_query` | `table_name` |
| Module-level mutation | `_module_<module>_mutation` | `insert_table` |
| Specific field on a type | `type_name` | `field_name` |

### Authentication variables (used in filters/defaults)
`[$auth.user_id]`, `[$auth.user_id_int]`, `[$auth.user_name]`, `[$auth.role]`, `[$auth.auth_type]`, `[$auth.provider]`, plus custom JWT claims like `[$auth.tenant_id]`
