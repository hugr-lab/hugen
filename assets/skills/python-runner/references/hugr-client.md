# Talking to Hugr from Python (hugr-client)

`hugr-client` ships in the bundled venv. It executes GraphQL against Hugr
and returns Arrow tables / pandas DataFrames / GeoDataFrames. Inside the
hugen runtime there is no `connections.json` and no JupyterLab — the
client reads `HUGR_URL` and `HUGR_TOKEN` from the env and runs headless.

## Construct a client — every call

`HugrClient()` reads `HUGR_URL` / `HUGR_TOKEN` from the env at
construction — no args needed (hugen injects them per spawn). The
runtime rotates `HUGR_TOKEN` between spawns, so **construct a fresh
client every call — never cache one at module level or across a sleep**
(a stale instance holds a rotated-out JWT and the next call 401s).

```python
from hugr import HugrClient

def hugr() -> HugrClient:
    return HugrClient()   # reads HUGR_URL / HUGR_TOKEN from the env
```

A short-lived helper is fine; persisting `HugrClient` across long sleeps
is not — by the time you wake up the JWT may have rotated and the next
call returns 401.

## Quick query

```python
client = hugr()
result = client.query("""
{
    core { data_sources { name type } }
}
""")

# pyarrow Table — zero-copy, most efficient. to_arrow() lives on the
# PART (no path arg); index the response by part path to reach it.
table = result["data.core.data_sources"].to_arrow()

# pandas DataFrame — fresh copy
df = result.df("data.core.data_sources")

# Single record (use _by_pk queries)
record = result.record("data.core.data_source_by_pk")
```

The first arg is the **part path**: `data.<top-level-field>` for a query
field at the root, deeper for nested modules. Hugr returns a multipart
response — one part per top-level field, even when one query selects many.

```python
result = client.query("{ devices { id name } drivers { id name } }")
result.df("data.devices")
result.df("data.drivers")
```

## Variables

```python
result = client.query(
    "query($id: BigInt!) { device_by_pk(id: $id) { id name } }",
    variables={"id": 42},
)
df = result.df("data.device_by_pk")
```

## Geometry

Hugr's geometry fields auto-decode through `to_geo_dataframe`:

```python
gdf = result.parts["data.devices"].to_geo_dataframe("geom")
print(gdf.crs)            # EPSG:4326
gdf.plot()                # geopandas .plot() works headless
```

Nested geometry (one-to-one or one-to-many relation):

```python
# devices nested under drivers; flatten on the geometry field
gdf = result.gdf("data.drivers", "devices.geom")
```

GeoJSON export (FeatureCollection per part):

```python
layers = result.parts["data.devices"].geojson_layers()
```

## Streaming (large result sets)

For >100k-row result sets that won't fit comfortably in memory, use the
WebSocket streaming API — Arrow batches arrive incrementally:

```python
import asyncio
from hugr import connect_stream

async def main():
    client = connect_stream(
        url=os.environ["HUGR_URL"],
        token=os.environ["HUGR_TOKEN"],
    )
    async with await client.stream("{ events { id ts payload } }") as stream:
        async for batch in stream.chunks():
            # batch: pyarrow.RecordBatch
            ...

asyncio.run(main())
```

Methods on the stream: `.chunks()` (RecordBatch async-iter), `.rows()`
(dict async-iter), `.to_pandas()` (collect-all), `.count()`.

In hugen the streaming path is rarely the right tool — `hugr-query:query`
already persists big result sets as Parquet, and reading Parquet is
faster than streaming GraphQL. Use Python streaming only when your
transform is genuinely batch-by-batch (incremental ML training, real-time
aggregation) and can't run after a Parquet write.

## Subscriptions

WebSocket subscriptions deliver server-pushed events:

```python
import asyncio
from hugr import connect_stream

async def main():
    client = connect_stream(
        url=os.environ["HUGR_URL"],
        token=os.environ["HUGR_TOKEN"],
    )
    sub = await client.subscribe("""
        subscription {
            core { store { subscribe(store: "redis", channel: "events") {
                channel message
            }}}
        }
    """)
    async for event in sub.events():
        df = event.to_pandas()
        ...

asyncio.run(main())
```

## Roles

A query may need a non-default role:

```python
client = HugrClient(
    url=os.environ["HUGR_URL"],
    token=os.environ["HUGR_TOKEN"],
    role="analyst",
)
```

The role flows through the `X-Hugr-Role` header. RBAC at the Hugr server
filters the schema and result rows — same rules as everywhere else in
hugen.

## Common errors

- **`401 Unauthenticated`** — almost always a stale token. Construct a
  fresh `HugrClient()` at the call site (it re-reads the env), not one
  stashed in a global. If it persists, the rotation contract is broken
  at the runtime layer (operator problem).
- **`Cannot query field "X" on type "Query"`** — same trap as in raw
  GraphQL. `X` is a submodule (dotted-name nesting) or it's RBAC-gated.
  Run discovery via the `hugr-data` skill (or the `hugr-main` MCP) to
  see what's reachable for your role.
- **`HUGR_URL not set`** — no-Hugr deployment, or operator dropped the
  `tool_providers[].env.HUGR_URL` entry. Bail out with a clear message.
- **Errors vs. empty — guard for both.** `query()` **raises** on a
  failure: `ValueError` on any GraphQL error (a syntax error, an
  unknown field, a bad argument — the server returns these as an
  `errors` part and the client surfaces them) or HTTP 500;
  `PermissionError` on 401/403. A partial response (data + errors)
  comes back with the errors on `r.errors` (a list) — call
  `r.raise_for_errors()` to fail loud on those too. An RBAC-filtered or
  genuinely empty result is NOT an error: `query()` returns normally
  with `r.parts == {}` (or the requested part missing). So guard every
  fetch for BOTH — let `except` catch the raise, and a parts-check
  catch the legitimate empty:

  ```python
  try:
      r = c.query(gql, variables=vars)        # keep gql VERBATIM — do not retype
  except (ValueError, PermissionError) as e:  # GraphQL errors / HTTP 500 / 401-403
      raise SystemExit(f"query failed: {e}")
  if not r.parts or part_path not in r.parts:  # empty / RBAC-filtered (not an error → no raise)
      raise SystemExit(f"no data for {part_path}; parts={list(r.parts)}")
  ```

## Choosing between `hugr-client` and `hugr-query`

| Need | Tool |
|------|------|
| Result fits in memory; you want a DataFrame for transform/plot | `hugr-client` (this skill) |
| Result is large or you want it persisted as Parquet on disk | `hugr-query:query` (loaded via `hugr-data` skill) |
| Server-side JQ post-process to a single JSON value | `hugr-query:query_jq` |
| Subscription / streaming / per-row processing | `hugr-client.connect_stream` |

Don't pull a whole result into a DataFrame just to write it back to
Parquet — call `hugr-query:query` and read the Parquet directly.
