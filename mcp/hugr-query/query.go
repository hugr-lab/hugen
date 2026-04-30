package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/hugr-lab/query-engine/client"
	"github.com/hugr-lab/query-engine/types"
)

// queryDeps groups the values every tool handler needs. The
// runtime constructs one of these at boot and the handlers close
// over it.
type queryDeps struct {
	client    *client.Client
	timeouts  timeoutConfig
	workspace string // WORKSPACES_ROOT (parent of per-session scratch dirs)
	shared    string // SHARED_ROOT (optional)
	agentID   string // current agent id (used for /shared/<aid>/ resolution)
}

// queryArgs is the LLM-supplied input for hugr.Query.
type queryArgs struct {
	GraphQL   string         `json:"graphql"`
	Variables map[string]any `json:"variables,omitempty"`
	Path      string         `json:"path,omitempty"`
	Format    string         `json:"format,omitempty"`
	TimeoutMS int            `json:"timeout_ms,omitempty"`
}

// queryJQArgs is the LLM-supplied input for hugr.QueryJQ.
type queryJQArgs struct {
	GraphQL   string         `json:"graphql"`
	Variables map[string]any `json:"variables,omitempty"`
	JQ        string         `json:"jq"`
	Path      string         `json:"path,omitempty"`
	TimeoutMS int            `json:"timeout_ms,omitempty"`
}

// queryResult is the envelope returned to the LLM. The schema is
// stable: existing tests in `../agent` and the spec contract both
// reference these field names.
type queryResult struct {
	QueryID   string   `json:"query_id"`
	Path      string   `json:"path,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Format    string   `json:"format,omitempty"`
	RowCount  int      `json:"row_count,omitempty"`
	Preview   any      `json:"preview,omitempty"`
	ElapsedMS int      `json:"elapsed_ms"`
	Truncated bool     `json:"truncated,omitempty"`
}

const previewRowCap = 50

// runQuery is the hugr.Query handler. It runs the GraphQL, walks
// the top-level data map, writes one file per top-level field
// (Parquet for ArrowTable, JSON otherwise), and returns a
// queryResult with a short preview.
func (d *queryDeps) runQuery(ctx context.Context, sessionID string, in queryArgs) (queryResult, error) {
	if strings.TrimSpace(in.GraphQL) == "" {
		return queryResult{}, fmt.Errorf("graphql: empty")
	}
	deadline := d.timeouts.effectiveDeadline(in.TimeoutMS)
	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	start := time.Now()

	resp, err := d.client.Query(cctx, in.GraphQL, in.Variables)
	elapsed := time.Since(start)
	if err != nil {
		return queryResult{}, mapClientError(cctx, err, elapsed)
	}
	defer resp.Close()
	if errsList := resp.Errors; len(errsList) > 0 {
		return queryResult{}, hugrError(errsList)
	}

	queryID := newShortID()
	written, err := d.writeResponse(sessionID, in.Path, in.Format, queryID, resp)
	if err != nil {
		return queryResult{}, err
	}
	out := queryResult{
		QueryID:   queryID,
		ElapsedMS: int(elapsed / time.Millisecond),
	}
	switch len(written) {
	case 0:
		// No tabular data — leave path empty.
	case 1:
		out.Path = written[0].path
		out.Format = written[0].format
		out.RowCount = written[0].rowCount
		out.Preview = written[0].preview
		out.Truncated = written[0].truncated
	default:
		out.Paths = make([]string, len(written))
		for i, w := range written {
			out.Paths[i] = w.path
		}
		// Multi-output preview: a small map of field → preview.
		previews := make(map[string]any, len(written))
		for _, w := range written {
			previews[w.field] = w.preview
		}
		out.Preview = previews
	}
	return out, nil
}

// runQueryJQ is the hugr.QueryJQ handler. It runs the GraphQL via
// QueryJSON (which applies the JQ post-processor server-side) and
// writes the resulting JsonValue to a single .json file.
func (d *queryDeps) runQueryJQ(ctx context.Context, sessionID string, in queryJQArgs) (queryResult, error) {
	if strings.TrimSpace(in.GraphQL) == "" {
		return queryResult{}, fmt.Errorf("graphql: empty")
	}
	if strings.TrimSpace(in.JQ) == "" {
		return queryResult{}, fmt.Errorf("jq: empty")
	}
	deadline := d.timeouts.effectiveDeadline(in.TimeoutMS)
	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	start := time.Now()

	val, err := d.client.QueryJSON(cctx, types.JQRequest{
		JQ: in.JQ,
		Query: types.Request{
			Query:     in.GraphQL,
			Variables: in.Variables,
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		return queryResult{}, mapJQError(cctx, err, elapsed)
	}
	queryID := newShortID()
	path, err := d.resolveOutPath(sessionID, in.Path, queryID, "json")
	if err != nil {
		return queryResult{}, err
	}
	body := []byte(*val)
	if err := writeFileAtomic(path, body); err != nil {
		return queryResult{}, &toolError{Code: "io", Msg: err.Error()}
	}

	// Preview: parse and cap (best effort — JsonValue may be
	// any JSON value). On parse failure, surface raw bytes capped.
	var preview any
	if err := json.Unmarshal(body, &preview); err != nil {
		if len(body) > 4096 {
			preview = string(body[:4096])
		} else {
			preview = string(body)
		}
	}
	preview = capPreview(preview)
	return queryResult{
		QueryID:   queryID,
		Path:      path,
		Format:    "json",
		Preview:   preview,
		ElapsedMS: int(elapsed / time.Millisecond),
	}, nil
}

// writtenFile records one written output for the multi-output
// path. format is "parquet" or "json".
type writtenFile struct {
	field     string
	path      string
	format    string
	rowCount  int
	preview   any
	truncated bool
}

// writeResponse walks the top-level data map and writes one file
// per field. ArrowTable values become Parquet (default) or JSON
// when the LLM asked for `format: json`. Non-tabular values always
// become JSON regardless of `format` — the format hint only
// applies to tabular shapes.
func (d *queryDeps) writeResponse(sessionID, requestedPath, format, queryID string, resp *types.Response) ([]writtenFile, error) {
	if resp.Data == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(resp.Data))
	for k := range resp.Data {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if format == "" {
		format = "parquet"
	}
	multi := len(keys) > 1
	out := make([]writtenFile, 0, len(keys))
	for _, k := range keys {
		v := resp.Data[k]
		ext, isTable := pickExt(v, format)
		var path string
		var err error
		if multi || requestedPath == "" {
			path, err = d.defaultPath(sessionID, queryID+"_"+sanitizeKey(k), ext)
			if err != nil {
				return out, err
			}
		} else {
			path, err = d.resolveOutPath(sessionID, requestedPath, queryID, ext)
			if err != nil {
				return out, err
			}
		}
		if isTable && format == "parquet" {
			tbl, ok := v.(types.ArrowTable)
			if !ok {
				return out, fmt.Errorf("expected ArrowTable for %s, got %T", k, v)
			}
			rows, prev, err := writeParquet(path, tbl)
			if err != nil {
				return out, &toolError{Code: "io", Msg: err.Error()}
			}
			out = append(out, writtenFile{
				field: k, path: path, format: "parquet",
				rowCount: rows, preview: prev,
			})
			continue
		}
		// JSON path (either non-tabular or format=json).
		body, err := json.Marshal(jsonifiable(v))
		if err != nil {
			return out, &toolError{Code: "io", Msg: "marshal: " + err.Error()}
		}
		if err := writeFileAtomic(path, body); err != nil {
			return out, &toolError{Code: "io", Msg: err.Error()}
		}
		var preview any
		_ = json.Unmarshal(body, &preview)
		preview = capPreview(preview)
		out = append(out, writtenFile{
			field: k, path: path, format: "json",
			preview: preview,
		})
	}
	return out, nil
}

// pickExt decides the output format/extension for a single field
// value. Tabular shapes (ArrowTable) honour the format hint;
// scalars and objects always serialise to JSON.
func pickExt(v any, format string) (ext string, isTable bool) {
	if _, ok := v.(types.ArrowTable); ok {
		if format == "json" {
			return "json", true
		}
		return "parquet", true
	}
	return "json", false
}

// jsonifiable rewrites types that don't marshal naturally. JsonValue
// is already JSON; passing it through json.Marshal would re-quote.
func jsonifiable(v any) any {
	switch x := v.(type) {
	case *types.JsonValue:
		var out any
		if err := json.Unmarshal([]byte(*x), &out); err == nil {
			return out
		}
		return string(*x)
	case types.JsonValue:
		var out any
		if err := json.Unmarshal([]byte(x), &out); err == nil {
			return out
		}
		return string(x)
	}
	return v
}

func sanitizeKey(k string) string {
	repl := strings.NewReplacer("/", "_", "\\", "_", " ", "_", ".", "_")
	return repl.Replace(k)
}

// writeParquet streams an ArrowTable to a Parquet file. Returns
// the row count and a small JSON-marshalled preview (≤ 50 rows).
// The preview is constructed from a separate read-only pass over
// the in-memory Arrow data, so the writer can stream-and-release.
func writeParquet(path string, tbl types.ArrowTable) (rows int, preview any, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, nil, err
	}
	reader, err := tbl.Reader(true)
	if err != nil {
		return 0, nil, fmt.Errorf("arrow reader: %w", err)
	}
	defer reader.Release()

	if reader == nil {
		// Empty result — write an empty parquet file with no schema?
		// pqarrow needs a schema. Easier: write `[]` JSON sidecar
		// behaviour is over-engineering for empty results; touch
		// the path and return zero rows.
		if err := writeFileAtomic(path, []byte{}); err != nil {
			return 0, nil, err
		}
		return 0, []any{}, nil
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, nil, err
	}
	props := parquet.NewWriterProperties()
	arrProps := pqarrow.DefaultWriterProps()
	w, err := pqarrow.NewFileWriter(reader.Schema(), f, props, arrProps)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return 0, nil, fmt.Errorf("parquet writer: %w", err)
	}

	previewRows := make([]map[string]any, 0, previewRowCap)
	for reader.Next() {
		batch := reader.RecordBatch()
		if err := w.WriteBuffered(batch); err != nil {
			_ = w.Close()
			_ = f.Close()
			_ = os.Remove(tmp)
			return 0, nil, fmt.Errorf("parquet write: %w", err)
		}
		rows += int(batch.NumRows())
		// Sample first batches into preview until cap.
		if len(previewRows) < previewRowCap {
			previewRows = appendBatchPreview(previewRows, batch, previewRowCap)
		}
	}
	// pqarrow's Close flushes the footer AND closes the underlying
	// io.Writer when it's an io.WriteCloser (TellWrapper.Close
	// detects it). Calling f.Close again afterwards would yield
	// "file already closed".
	if err := w.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return 0, nil, fmt.Errorf("parquet close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, nil, err
	}
	preview = previewRows
	return rows, preview, nil
}

// appendBatchPreview converts a record batch's first N rows into
// JSON-marshallable maps. Arrow's RecordBatch.MarshalJSON emits
// `[{col: val, ...}, ...]` — we decode and slice to the cap.
func appendBatchPreview(into []map[string]any, batch arrow.RecordBatch, cap int) []map[string]any {
	if len(into) >= cap {
		return into
	}
	body, err := batch.MarshalJSON()
	if err != nil {
		return into
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		return into
	}
	for _, r := range rows {
		if len(into) >= cap {
			break
		}
		into = append(into, r)
	}
	return into
}
