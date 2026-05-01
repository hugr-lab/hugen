package main

import (
	"encoding/json"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
)

// arrowExtensionNameKey is the canonical Arrow field-metadata key
// used to advertise an extension type during IPC serialization.
// For *registered* extensions (arrow.ExtensionType), the name is
// also exposed via `Type.ExtensionName()` — we check both so the
// detector works regardless of whether DuckDB-Arrow registers
// its geoarrow types globally for this process.
const arrowExtensionNameKey = "ARROW:extension:name"

// extensionName returns the Arrow extension name attached to `f`,
// or "" when the field is plain. Two paths are checked because the
// engine produces both shapes in the wild:
//
//   - A registered extension type — `f.Type` implements
//     arrow.ExtensionType, the name is on the type itself.
//     This is the common case for geometry columns coming from
//     DuckDB-Arrow, which registers geoarrow.wkb at init.
//   - A plain storage type with field metadata — `f.Type` is
//     binary/string and `ARROW:extension:name` lives in
//     `f.Metadata`. The engine's `addGeometryFieldMeta` writes
//     this for `hugr.h3cell` / `hugr.geojson`, and any caller
//     that builds a schema by hand may use this shape.
func extensionName(f arrow.Field) string {
	if et, ok := f.Type.(arrow.ExtensionType); ok {
		return et.ExtensionName()
	}
	if i := f.Metadata.FindKey(arrowExtensionNameKey); i >= 0 {
		return f.Metadata.Values()[i]
	}
	return ""
}

// extensionMeta returns the extension's serialised JSON metadata
// (the GeoArrow-spec payload with crs/edges/etc.), or "" when the
// field has none. Same dual-source rule as extensionName.
func extensionMeta(f arrow.Field) string {
	if et, ok := f.Type.(arrow.ExtensionType); ok {
		return et.Serialize()
	}
	if i := f.Metadata.FindKey("ARROW:extension:metadata"); i >= 0 {
		return f.Metadata.Values()[i]
	}
	return ""
}

// isMeaningful reports whether the raw JSON value is something
// other than `null`/empty. We use this to decide whether to copy
// `crs` through to the GeoParquet metadata — explicit null in
// GeoParquet means "unknown CRS", which is different from
// "omitted CRS" (which means OGC:CRS84 per spec).
func isMeaningful(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// geoParquetVersion is the GeoParquet spec version we declare in
// the file-level `geo` metadata. 1.1.0 is the current published
// spec and is what DuckDB ≥1.5 reads.
const geoParquetVersion = "1.1.0"

// buildGeoFileMetadata scans `s` for fields tagged with the
// `geoarrow.wkb` Arrow extension type and returns the JSON payload
// to attach as the file-level `geo` key per the GeoParquet 1.1
// spec. Returns "" when no geometry columns are present (so the
// caller can skip the AppendKeyValueMetadata call).
//
// The payload is intentionally minimal:
//
//   - `encoding`: "WKB" (spec-required).
//   - `geometry_types`: [] (spec-required, but spec explicitly
//     allows the empty array "if they are not known" — we don't
//     scan WKB to enumerate them, that's what readers do).
//   - `bbox` is OPTIONAL per spec; we don't compute it (would mean
//     decoding every WKB blob — pure overhead for the writer).
//
// `primary_column` is set to the first geometry column encountered
// in declaration order, which matches what DuckDB and GeoPandas
// pick when authors don't specify one explicitly.
func buildGeoFileMetadata(s *arrow.Schema) string {
	if s == nil {
		return ""
	}
	type colMeta struct {
		Encoding      string          `json:"encoding"`
		GeometryTypes []string        `json:"geometry_types"`
		CRS           json.RawMessage `json:"crs,omitempty"`
		Edges         string          `json:"edges,omitempty"`
	}
	type fileMeta struct {
		Version       string             `json:"version"`
		PrimaryColumn string             `json:"primary_column"`
		Columns       map[string]colMeta `json:"columns"`
	}
	cols := map[string]colMeta{}
	var primary string
	for _, f := range s.Fields() {
		if extensionName(f) != "geoarrow.wkb" {
			continue
		}
		if primary == "" {
			primary = f.Name
		}
		col := colMeta{
			Encoding:      "WKB",
			GeometryTypes: []string{},
		}
		// GeoArrow and GeoParquet 1.1 use the same JSON shape for
		// `crs` and `edges` — pass through directly when geoarrow
		// filled them. We do drop `crs: null` so the reader falls
		// back to the spec default (OGC:CRS84) instead of treating
		// the column as having an explicitly-unknown CRS.
		if raw := extensionMeta(f); raw != "" {
			var ext struct {
				CRS   json.RawMessage `json:"crs"`
				Edges string          `json:"edges"`
			}
			if err := json.Unmarshal([]byte(raw), &ext); err == nil {
				if isMeaningful(ext.CRS) {
					col.CRS = ext.CRS
				}
				if ext.Edges != "" {
					col.Edges = ext.Edges
				}
			}
		}
		cols[f.Name] = col
	}
	if len(cols) == 0 {
		return ""
	}
	body, err := json.Marshal(fileMeta{
		Version:       geoParquetVersion,
		PrimaryColumn: primary,
		Columns:       cols,
	})
	if err != nil {
		return ""
	}
	return string(body)
}
