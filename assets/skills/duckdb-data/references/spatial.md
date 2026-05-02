# Geospatial operations (ST_*)

> Adapted from upstream
> [`duckdb/duckdb-skills/skills/spatial/`](https://github.com/duckdb/duckdb-skills/tree/main/skills/spatial)
> (MIT). Per-call `INSTALL spatial; LOAD spatial;` dropped — the
> operator pre-loads it via `--init-sql` (see Critical Rules in
> `SKILL.md`). Overture Maps content kept where useful.

## Pre-loaded extensions

`spatial` is loaded by the operator at provider startup. Skill content
must NOT call `INSTALL spatial` or `LOAD spatial` per query — it's
wasted I/O and the skill is forbidden from doing so.

`h3` (community extension for hexagonal binning) is NOT pre-loaded; if
the user explicitly asks for hex aggregation, pay the install once:

```sql
INSTALL h3 FROM community; LOAD h3;
```

## Always set XY

```sql
SET geometry_always_xy = true;
```

This makes every spatial function read coordinates as `(longitude,
latitude)` — the convention for GeoJSON, Overture Maps, and the
`POINT(lng lat)` WKT form. Without it, `ST_Distance_Spheroid` and
friends assume `(lat, lng)` and silently return wrong numbers.

Run it once per session, before the first ST_* call, in the same
`execute_query` batch as your query if you forgot earlier.

## Distance: planar vs spheroid

| Function                                          | Units                  | When                                     |
|---------------------------------------------------|------------------------|------------------------------------------|
| `ST_Distance(a, b)`                               | input CRS units        | local-plane geometries (UTM, EPSG:3857)  |
| `ST_Distance_Spheroid(a, b)`                      | metres on WGS-84       | lat/lng inputs, real-world distances     |
| `ST_DWithin_Spheroid(a, b, m)`                    | bool                   | "within m metres" filters                |

`ST_Distance_Spheroid` requires `POINT_2D` arguments. Generic `GEOMETRY`
or `GEOMETRY('OGC:CRS84')` columns must be cast first by extracting
coordinates:

```sql
ST_Point(ST_X(geometry), ST_Y(geometry))::POINT_2D
```

## Common idioms

### Find rows within X km of a reference point

```sql
SET geometry_always_xy = true;
WITH ref AS (
  SELECT ST_Point(13.4050, 52.5200)::POINT_2D AS pt   -- Berlin
)
SELECT name,
       ST_Distance_Spheroid(
         ref.pt,
         ST_Point(ST_X(geom), ST_Y(geom))::POINT_2D
       ) AS m
FROM read_parquet('data/cities.parquet'), ref
WHERE m < 50000
ORDER BY m;
```

### Point-in-polygon

```sql
SELECT p.id, c.name AS country
FROM points p
JOIN countries c
  ON ST_Contains(c.geom, p.geom);
```

### Read GeoJSON

```sql
SELECT name, ST_AsText(geom) AS wkt
FROM st_read('data/places.geojson')
LIMIT 10;
```

`st_read` is the universal GDAL reader; it handles GeoJSON, Shapefile,
GeoPackage, GPX, KML, FlatGeobuf, etc. by extension.

### Convert tabular lat/lng to geometry

```sql
CREATE OR REPLACE TABLE points AS
  SELECT id, name,
         ST_Point(lng, lat) AS geom
  FROM read_csv_auto('data/poi.csv');
```

CSV columns named `latitude` / `longitude` are common — note the
`ST_Point(lng, lat)` argument order (XY).

### Export to GeoJSON

```sql
COPY (SELECT name, geom FROM points)
  TO 'reports/places.geojson'
  (FORMAT GDAL, DRIVER 'GeoJSON');
```

Convention: maps and visual artefacts live under `reports/`; data files
under `data/`.

## H3 hex binning (when needed)

```sql
INSTALL h3 FROM community; LOAD h3;
SELECT h3_h3_to_string(h3_latlng_to_cell(lat, lng, 7)) AS hex,
       count() AS n
FROM events
GROUP BY 1
ORDER BY n DESC
LIMIT 100;
```

Resolution 7 ≈ 5 km hexes; resolution 9 ≈ 200 m. See the H3 docs for
the full ladder.

## Overture Maps (free global POI / building / road data)

Public buckets, no key needed. The cardinal rule is **bbox filter
first** — Overture Parquet has bbox columns (`bbox.xmin/xmax/ymin/ymax`)
and DuckDB pushes the filter into the file reader:

```sql
SET geometry_always_xy = true;
SELECT names.primary AS name, geometry
FROM read_parquet('s3://overturemaps-us-west-2/release/.../places/*.parquet')
WHERE bbox.xmin BETWEEN 13.30 AND 13.50
  AND bbox.ymin BETWEEN 52.45 AND 52.55
  AND categories.primary = 'restaurant'
LIMIT 200;
```

Skip the bbox filter and you'll download tens of GB.

## Common errors

| Symptom                                                | Fix                                                                                |
|--------------------------------------------------------|------------------------------------------------------------------------------------|
| `Function ST_Distance_Spheroid does not exist for ...` | Cast inputs to `POINT_2D` (extract X/Y first)                                      |
| Distances off by a factor of ~111                      | You forgot `SET geometry_always_xy = true;` — coordinates parsed as (lat, lng)     |
| `Catalog Error: Function st_read ...`                  | The `spatial` extension didn't load — operator's `--init-sql` is broken; report it |
| `Could not parse geometry`                             | The column is WKT/WKB but DuckDB read it as text — wrap in `ST_GeomFromText(...)`  |
