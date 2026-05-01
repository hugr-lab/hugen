package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/hugr-lab/query-engine/types"
)

// fakeTable wraps a single RecordBatch into types.ArrowTable. Used
// to avoid dragging the full ArrowTableChunked machinery into a
// unit test — Reader returns a one-batch RecordReader.
func makeArrowTable(t *testing.T) types.ArrowTable {
	t.Helper()
	mem := memory.NewGoAllocator()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "name", Type: arrow.BinaryTypes.String},
	}, nil)
	b := array.NewRecordBuilder(mem, schema)
	defer b.Release()
	b.Field(0).(*array.Int64Builder).AppendValues([]int64{1, 2, 3}, nil)
	b.Field(1).(*array.StringBuilder).AppendValues([]string{"alice", "bob", "carol"}, nil)
	rec := b.NewRecord()
	defer rec.Release()
	tbl := types.NewArrowTable()
	tbl.Append(rec)
	return tbl
}

func TestWriteResponse_TabularToParquet(t *testing.T) {
	d, ws := newDeps(t)
	resp := &types.Response{
		Data: map[string]any{"customers": makeArrowTable(t)},
	}
	written, err := d.writeResponse("sess1", "", "qid", resp)
	if err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("written=%d", len(written))
	}
	w := written[0]
	if w.Format != "parquet" {
		t.Fatalf("format=%s", w.Format)
	}
	if w.RowCount != 3 {
		t.Fatalf("rowCount=%d want 3", w.RowCount)
	}
	if len(w.Schema) != 2 {
		t.Fatalf("schema cols=%d want 2", len(w.Schema))
	}
	if w.Schema[0].Name != "id" || w.Schema[1].Name != "name" {
		t.Fatalf("schema names = %+v", w.Schema)
	}

	// Validate the file exists under <session>/data/<queryID>/ and
	// is a real Parquet (not a sentinel). Default-dir layout puts
	// each part inside the per-call directory keyed by the
	// sanitized GraphQL field path.
	want := filepath.Join(ws, "sess1", "data", "qid", "customers.parquet")
	if w.Path != want {
		t.Fatalf("path=%s want %s", w.Path, want)
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatalf("stat: %v", err)
	}
	pf, err := file.OpenParquetFile(w.Path, false)
	if err != nil {
		t.Fatalf("open parquet: %v", err)
	}
	defer pf.Close()
	if pf.NumRows() != 3 {
		t.Fatalf("parquet rows=%d want 3", pf.NumRows())
	}
}

func TestWriteResponse_ScalarTopLevelGoesToJSON(t *testing.T) {
	d, ws := newDeps(t)
	resp := &types.Response{
		Data: map[string]any{"count": 42.0},
	}
	written, err := d.writeResponse("sess1", "", "qid", resp)
	if err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("written=%d", len(written))
	}
	w := written[0]
	if w.Format != "json" {
		t.Fatalf("format=%s want json", w.Format)
	}
	body, err := os.ReadFile(w.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "42" {
		t.Fatalf("file=%q want 42", string(body))
	}
	if !filepath.IsAbs(w.Path) || filepath.Dir(w.Path) != filepath.Join(ws, "sess1", "data", "qid") {
		t.Fatalf("path=%s not under <sid>/data/<qid>/", w.Path)
	}
	if w.Preview != "42" {
		t.Fatalf("preview=%q want 42", w.Preview)
	}
}

func TestWriteResponse_MultiOutput(t *testing.T) {
	d, _ := newDeps(t)
	resp := &types.Response{
		Data: map[string]any{
			"customers": makeArrowTable(t),
			"summary":   map[string]any{"total": 3},
		},
	}
	written, err := d.writeResponse("sess1", "", "qid", resp)
	if err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("written=%d", len(written))
	}
	// Each output uses a distinct path (key embedded).
	paths := map[string]bool{}
	for _, w := range written {
		paths[w.Path] = true
	}
	if len(paths) != 2 {
		t.Fatalf("paths collide: %v", paths)
	}
}

func TestRunQueryJQ_ValidatesEmptyArgs(t *testing.T) {
	d, _ := newDeps(t)
	_, err := d.runQueryJQ(t.Context(), "sess1", queryJQArgs{})
	if err == nil {
		t.Fatal("expected error on empty graphql")
	}
	_, err = d.runQueryJQ(t.Context(), "sess1", queryJQArgs{GraphQL: "{ foo }"})
	if err == nil {
		t.Fatal("expected error on empty jq")
	}
}

func TestRunQuery_ValidatesEmptyGraphql(t *testing.T) {
	d, _ := newDeps(t)
	_, err := d.runQuery(t.Context(), "sess1", queryArgs{})
	if err == nil {
		t.Fatal("expected error on empty graphql")
	}
}

func TestQueryResultEnvelope_ShapeStable(t *testing.T) {
	out := queryResult{
		QueryID: "qid",
		Part: &partEntry{
			Path:     "/x.parquet",
			Format:   "parquet",
			Part:     "customers",
			Size:     1234,
			RowCount: 10,
			Schema: []schemaColumn{
				{Name: "id", Type: "int64"},
				{Name: "name", Type: "utf8"},
			},
		},
		ElapsedMS: 7,
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"query_id", "part", "elapsed_ms"} {
		if _, ok := parsed[k]; !ok {
			t.Fatalf("missing key %q", k)
		}
	}
	p, ok := parsed["part"].(map[string]any)
	if !ok {
		t.Fatalf("part = %T", parsed["part"])
	}
	for _, k := range []string{"path", "format", "row_count", "schema", "part", "size"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("missing part.%s", k)
		}
	}
}

func TestToolError_ErrorsAs(t *testing.T) {
	wrapped := &toolError{Code: "timeout"}
	var te *toolError
	if !errors.As(wrapped, &te) || te.Code != "timeout" {
		t.Fatal("errors.As lost code")
	}
}
