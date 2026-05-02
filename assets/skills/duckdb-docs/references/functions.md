# DuckDB function categories

> Quick orientation. For the canonical full list see
> <https://duckdb.org/docs/sql/functions/overview>.

DuckDB groups functions by category. The ones below cover ~95 % of
analytical-SQL needs; obscure functions are best looked up by name in
the docs.

## Numeric

`abs`, `ceil`, `floor`, `round`, `sign`, `sqrt`, `exp`, `ln`, `log`,
`log10`, `pow(a,b)`, `random()`, `setseed(s)`, `mod(a,b)`, `gcd`, `lcm`.

Trig: `sin`, `cos`, `tan`, `asin`, `acos`, `atan`, `atan2`.

Generators: `range(start, end, step)`, `generate_series(...)`.

## String

`length`, `lower`, `upper`, `trim` / `ltrim` / `rtrim`, `lpad` / `rpad`,
`replace`, `reverse`, `repeat`, `substring(s, from, len)`,
`split_part(s, sep, n)`, `starts_with` / `ends_with` / `contains`,
`like`, `ilike`, `regexp_matches`, `regexp_extract`, `regexp_replace`,
`format('{}={}', k, v)`, `concat_ws(sep, ...)`, `string_split(s, sep)`,
`string_split_regex(s, pattern)`.

`printf('%05d', n)` is also available.

## Date / time

Types: `DATE`, `TIME`, `TIMESTAMP`, `TIMESTAMP WITH TIME ZONE`,
`INTERVAL`.

Constructors: `DATE '2026-05-01'`, `TIMESTAMP '2026-05-01 12:30:00'`,
`current_date`, `current_timestamp`, `today()`, `make_date(y,m,d)`,
`make_timestamp(...)`.

Extract / format: `extract(year FROM ts)`, `date_part('hour', ts)`,
`date_trunc('day', ts)`, `strftime(ts, '%Y-%m-%d')`, `strptime(s, '%Y-%m-%d')`.

Arithmetic: `ts + INTERVAL '1 day'`, `ts - INTERVAL 7 DAY`,
`age(t1, t2)`, `datediff('day', t1, t2)`, `date_add(t, INTERVAL x)`.

## Aggregate

Standard: `count()`, `sum`, `avg`, `min`, `max`, `median`, `mode`,
`stddev_samp`, `stddev_pop`, `variance`, `var_pop`.

Approximate / advanced: `approx_count_distinct`, `approx_quantile(col, q)`,
`quantile_cont(col, q)`, `quantile_disc(col, q)`, `histogram(col)`,
`first(col)`, `last(col)`, `arg_max(arg, by)`, `arg_min(arg, by)`,
`bit_and`, `bit_or`, `bit_xor`, `bool_and`, `bool_or`,
`string_agg(col, sep)`, `array_agg(col)`, `list(col)`.

DuckDB-special: `max(col, n)` and `min(col, n)` return top / bottom-n
as a list; `arg_max(arg, val, n)` returns top-n args by val.

## Window

`OVER (PARTITION BY ... ORDER BY ... ROWS / RANGE BETWEEN ... AND ...)`.

Functions that work with `OVER`: every aggregate above, plus
`row_number()`, `rank()`, `dense_rank()`, `percent_rank()`,
`cume_dist()`, `ntile(n)`, `lag(col, n)`, `lead(col, n)`,
`first_value(col)`, `last_value(col)`, `nth_value(col, n)`.

Frame defaults: omitting `ROWS` / `RANGE` defaults to `RANGE BETWEEN
UNBOUNDED PRECEDING AND CURRENT ROW` — usually NOT what you want for
running totals; specify `ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT
ROW` explicitly.

## Conditional

`coalesce(a, b, c, ...)`, `nullif(a, b)`, `ifnull(a, b)`,
`if(cond, a, b)`, `CASE WHEN cond THEN a ELSE b END`.

`try_cast(x AS DOUBLE)` — returns `NULL` instead of erroring on bad
input. Standard `CAST(x AS DOUBLE)` raises.

## JSON

Operate on `JSON` typed columns or strings.

`json_extract(j, '$.path')`, `json_extract_string`,
`json_extract_path_text`, `json_keys`, `json_array_length`, `json_type`,
`json_contains(haystack, needle)`, `json_object('k1', v1, ...)`.

The `->`/`->>` operators work too: `j -> 'key'`, `j ->> 'key'`.

`read_json_auto` / `read_json` for files; `to_json(struct)` to render.

## List / struct / map

Lists: `list_value(...)`, `[1,2,3]`, `list_contains`, `list_append`,
`list_prepend`, `list_distinct`, `list_concat`, `list_sort`,
`list_reverse`, `list_filter(l, x -> x > 0)`, `list_transform(l, x -> x*2)`,
`list_aggregate(l, 'sum')`, `unnest(l)`.

Structs: `{'a': 1, 'b': 2}`, `s.a`, `struct_extract(s, 'a')`,
`struct_pack(a := 1, b := 2)`.

Maps: `map([1,2], ['a','b'])`, `m[1]`, `map_keys(m)`, `map_values(m)`.

## Cast / type-check

`CAST(x AS T)`, `x::T`, `TRY_CAST(x AS T)`, `typeof(x)`, `pg_typeof(x)`.
