package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/hugr-lab/query-engine/types"
)

// fakeExt is a minimal arrow.ExtensionType used to mimic the
// `extension<geoarrow.wkb>` type that DuckDB-Arrow attaches to
// geometry columns at IPC time. We can't pull in DuckDB's type
// registry from a unit test, so we just satisfy the interface
// directly — that's exactly what the schema-walker sees on the
// wire too.
type fakeExt struct {
	arrow.ExtensionBase
	name       string
	serialized string
}

func newFakeExt(name string) *fakeExt {
	return &fakeExt{
		ExtensionBase: arrow.ExtensionBase{Storage: arrow.BinaryTypes.Binary},
		name:          name,
	}
}

func newFakeExtWithMeta(name, serialized string) *fakeExt {
	e := newFakeExt(name)
	e.serialized = serialized
	return e
}

func (e *fakeExt) ArrayType() reflect.Type                 { return reflect.TypeOf(array.ExtensionArrayBase{}) }
func (e *fakeExt) ExtensionName() string                   { return e.name }
func (e *fakeExt) ExtensionEquals(o arrow.ExtensionType) bool {
	return e.name == o.ExtensionName()
}
func (e *fakeExt) Serialize() string                                                  { return e.serialized }
func (e *fakeExt) Deserialize(arrow.DataType, string) (arrow.ExtensionType, error)    { return e, nil }

func TestBuildGeoFileMetadata_NoGeometry(t *testing.T) {
	s := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "name", Type: arrow.BinaryTypes.String},
	}, nil)
	if got := buildGeoFileMetadata(s); got != "" {
		t.Fatalf("expected empty for non-geo schema, got %q", got)
	}
}

func TestBuildGeoFileMetadata_SingleGeomColumn_RegisteredExtType(t *testing.T) {
	// Real wire shape: f.Type is *arrow.ExtensionType with
	// ExtensionName()="geoarrow.wkb". This is what DuckDB-Arrow
	// emits and what live hugr-query traffic looks like.
	s := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "geom", Type: newFakeExt("geoarrow.wkb")},
	}, nil)
	got := buildGeoFileMetadata(s)
	if got == "" {
		t.Fatal("expected non-empty geo metadata")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["version"] != "1.1.0" {
		t.Fatalf("version=%v", parsed["version"])
	}
	if parsed["primary_column"] != "geom" {
		t.Fatalf("primary_column=%v", parsed["primary_column"])
	}
	cols, _ := parsed["columns"].(map[string]any)
	geom, _ := cols["geom"].(map[string]any)
	if geom["encoding"] != "WKB" {
		t.Fatalf("encoding=%v", geom["encoding"])
	}
	gt, _ := geom["geometry_types"].([]any)
	if gt == nil || len(gt) != 0 {
		t.Fatalf("geometry_types=%v want empty slice", geom["geometry_types"])
	}
	if _, ok := geom["bbox"]; ok {
		t.Fatal("bbox should not be present (writer doesn't compute it)")
	}
}

func TestBuildGeoFileMetadata_SingleGeomColumn_FieldMetadata(t *testing.T) {
	// Alternate shape: plain binary column carrying the extension
	// name in field metadata only (no registered extension type).
	// Some engine code paths produce this shape — we still want
	// to detect them as geometry.
	geomMD := arrow.NewMetadata(
		[]string{"ARROW:extension:name", "ARROW:extension:metadata"},
		[]string{"geoarrow.wkb", `{"crs":null}`},
	)
	s := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "geom", Type: arrow.BinaryTypes.Binary, Metadata: geomMD},
	}, nil)
	got := buildGeoFileMetadata(s)
	if got == "" {
		t.Fatal("expected non-empty geo metadata for metadata-only path")
	}
}

func TestBuildGeoFileMetadata_MultipleGeomColumns(t *testing.T) {
	s := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "boundary", Type: newFakeExt("geoarrow.wkb")},
		{Name: "centroid", Type: newFakeExt("geoarrow.wkb")},
	}, nil)
	got := buildGeoFileMetadata(s)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["primary_column"] != "boundary" {
		t.Fatalf("primary_column=%v want boundary (first geom column)", parsed["primary_column"])
	}
	cols, _ := parsed["columns"].(map[string]any)
	if _, ok := cols["boundary"]; !ok {
		t.Fatal("missing boundary in columns")
	}
	if _, ok := cols["centroid"]; !ok {
		t.Fatal("missing centroid in columns")
	}
}

func TestBuildGeoFileMetadata_PassesThroughCRS(t *testing.T) {
	// PROJJSON object on geoarrow side — must round-trip into the
	// GeoParquet column metadata verbatim, no conversion needed.
	projjson := `{"$schema":"https://proj.org/schemas/v0.5/projjson.schema.json","type":"GeographicCRS","name":"WGS 84"}`
	serialized := `{"crs":` + projjson + `,"edges":"planar"}`
	s := arrow.NewSchema([]arrow.Field{
		{Name: "geom", Type: newFakeExtWithMeta("geoarrow.wkb", serialized)},
	}, nil)
	got := buildGeoFileMetadata(s)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	cols := parsed["columns"].(map[string]any)
	geom := cols["geom"].(map[string]any)
	if geom["edges"] != "planar" {
		t.Fatalf("edges=%v", geom["edges"])
	}
	crs, ok := geom["crs"].(map[string]any)
	if !ok {
		t.Fatalf("crs missing or wrong type: %T %v", geom["crs"], geom["crs"])
	}
	if crs["name"] != "WGS 84" {
		t.Fatalf("crs.name=%v", crs["name"])
	}
}

func TestBuildGeoFileMetadata_DropsNullCRS(t *testing.T) {
	// `crs: null` must be OMITTED from the output. Spec defaults
	// to OGC:CRS84 when crs is absent; emitting explicit null
	// would tell the reader the CRS is "unknown" instead.
	s := arrow.NewSchema([]arrow.Field{
		{Name: "geom", Type: newFakeExtWithMeta("geoarrow.wkb", `{"crs":null}`)},
	}, nil)
	got := buildGeoFileMetadata(s)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	geom := parsed["columns"].(map[string]any)["geom"].(map[string]any)
	if _, present := geom["crs"]; present {
		t.Fatalf("crs should be omitted when geoarrow says null, got %v", geom)
	}
}

func TestBuildGeoFileMetadata_IgnoresOtherExtensions(t *testing.T) {
	// Other ARROW extension types (e.g. uuid, fixed-shape tensor)
	// must NOT be promoted to geometry columns.
	md := arrow.NewMetadata(
		[]string{"ARROW:extension:name"},
		[]string{"arrow.uuid"},
	)
	s := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.BinaryTypes.Binary, Metadata: md},
	}, nil)
	if got := buildGeoFileMetadata(s); got != "" {
		t.Fatalf("non-geo extension should be ignored, got %q", got)
	}
}

// TestWriteParquet_EmitsGeoMetadata builds a one-batch table that
// has a wkb-tagged column and verifies the resulting Parquet file
// carries the file-level "geo" key. This is the round-trip a
// downstream reader (DuckDB spatial, geopandas, …) relies on to
// recognise the column as geometry.
func TestWriteParquet_EmitsGeoMetadata(t *testing.T) {
	mem := memory.NewGoAllocator()
	geomMD := arrow.NewMetadata(
		[]string{"ARROW:extension:name"},
		[]string{"geoarrow.wkb"},
	)
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "geom", Type: arrow.BinaryTypes.Binary, Metadata: geomMD},
	}, nil)
	b := array.NewRecordBuilder(mem, schema)
	defer b.Release()
	b.Field(0).(*array.Int64Builder).AppendValues([]int64{1, 2}, nil)
	// Two valid 2-byte WKB-ish blobs — content irrelevant, the
	// writer doesn't decode them.
	b.Field(1).(*array.BinaryBuilder).AppendValues([][]byte{{0x01, 0x02}, {0x03, 0x04}}, nil)
	rec := b.NewRecord()
	defer rec.Release()
	tbl := types.NewArrowTable()
	tbl.Append(rec)

	dir := t.TempDir()
	path := filepath.Join(dir, "out.parquet")
	rows, _, err := writeParquet(path, tbl)
	if err != nil {
		t.Fatalf("writeParquet: %v", err)
	}
	if rows != 2 {
		t.Fatalf("rows=%d want 2", rows)
	}

	pf, err := file.OpenParquetFile(path, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pf.Close()
	kv := pf.MetaData().KeyValueMetadata()
	val := kv.FindValue("geo")
	if val == nil {
		t.Fatalf("geo key missing from %v", kv.Keys())
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(*val), &parsed); err != nil {
		t.Fatalf("unmarshal geo: %v", err)
	}
	if parsed["primary_column"] != "geom" {
		t.Fatalf("primary_column=%v", parsed["primary_column"])
	}
}

func TestWriteParquet_NoGeoMetadataWhenAbsent(t *testing.T) {
	// Plain table → no "geo" key. Otherwise downstream readers
	// would refuse to treat the file as a non-spatial Parquet.
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.parquet")
	rows, _, err := writeParquet(path, makeArrowTable(t))
	if err != nil {
		t.Fatalf("writeParquet: %v", err)
	}
	if rows != 3 {
		t.Fatalf("rows=%d", rows)
	}
	pf, err := file.OpenParquetFile(path, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pf.Close()
	if v := pf.MetaData().KeyValueMetadata().FindValue("geo"); v != nil {
		t.Fatalf("geo key should be absent on plain table, got %s", *v)
	}
}
