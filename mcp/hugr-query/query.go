package main

import (
	"context"
	"encoding/json"
	"errors"
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
}

// queryArgs is the LLM-supplied input for hugr.Query. There is no
// `format` knob: tabular GraphQL leaves are always written as
// Parquet, object leaves as JSON. The split is decided by the
// engine response, not by the caller.
type queryArgs struct {
	GraphQL   string         `json:"graphql"`
	Variables map[string]any `json:"variables,omitempty"`
	Path      string         `json:"path,omitempty"`
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

// partEntry is the per-part descriptor returned to the LLM. The
// envelope shape lets the model reason about each output without
// having to read it back from disk:
//
//   - Path is the absolute on-disk path to the written file.
//   - Format is "parquet" (tabular) or "json" (object/extensions).
//   - Field is the dotted GraphQL path the data originated from
//     (e.g. "function.core.payments.aggregation").
//   - For Parquet: RowCount + Schema (column name, type, optional
//     metadata) — no row data preview, the file is the data.
//   - For JSON: Preview is a short text snippet (≤ jsonPreviewCap
//     bytes); long bodies set Truncated=true.
type partEntry struct {
	Path      string         `json:"path,omitempty"`
	Format    string         `json:"format,omitempty"`
	Part      string         `json:"part,omitempty"`
	Size      int64          `json:"size,omitempty"` // bytes written to disk
	RowCount  int            `json:"row_count,omitempty"`
	Schema    []schemaColumn `json:"schema,omitempty"`
	Preview   string         `json:"preview,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
	// Null is set when the GraphQL part is explicitly null or
	// empty. No file is written in that case — the envelope just
	// records the field so the model can reason about the shape
	// of the response without a missing-part surprise.
	Null bool `json:"null,omitempty"`
}

// schemaColumn describes one column of a Parquet output.
type schemaColumn struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// queryResult is the envelope returned to the LLM. Single-part
// responses fill `part`; multi-part responses fill `parts`.
type queryResult struct {
	QueryID   string      `json:"query_id"`
	Part      *partEntry  `json:"part,omitempty"`
	Parts     []partEntry `json:"parts,omitempty"`
	ElapsedMS int         `json:"elapsed_ms"`
}

// jsonPreviewCap caps the inline preview at 1 KB. The model reads
// the full file through bash-mcp when it needs more — the preview
// is just a quick glance to confirm the call landed.
const jsonPreviewCap = 1024

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
	written, err := d.writeResponse(sessionID, in.Path, queryID, resp)
	if err != nil {
		return queryResult{}, err
	}
	return assembleResult(queryID, elapsed, written), nil
}

// assembleResult collapses the per-part entries into the final
// queryResult envelope. Single-part → Part; multi-part → Parts.
func assembleResult(queryID string, elapsed time.Duration, written []partEntry) queryResult {
	out := queryResult{
		QueryID:   queryID,
		ElapsedMS: int(elapsed / time.Millisecond),
	}
	switch len(written) {
	case 0:
		// Empty response — leave the part slots unset.
	case 1:
		entry := written[0]
		out.Part = &entry
	default:
		out.Parts = written
	}
	return out
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
	dir, err := d.resolveOutDir(sessionID, in.Path, queryID)
	if err != nil {
		return queryResult{}, err
	}
	path := filepath.Join(dir, queryID+".json")
	body := []byte(*val)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return queryResult{}, &toolError{Code: "io", Msg: err.Error()}
	}
	preview, truncated := jsonTextPreview(body)
	return queryResult{
		QueryID: queryID,
		Part: &partEntry{
			Path:      path,
			Format:    "json",
			Size:      int64(len(body)),
			Preview:   preview,
			Truncated: truncated,
		},
		ElapsedMS: int(elapsed / time.Millisecond),
	}, nil
}

// writeResponse walks the response via the multipart-aware
// types.Response API and writes one file per part into a single
// output directory chosen by the caller:
//
//   - resp.Tables()    — ArrowTable parts → Parquet.
//   - resp.Objects()   — non-table parts (typically *JsonValue) → JSON.
//   - resp.Extensions  — written verbatim as JSON when present.
//
// `requestedDir` is the LLM-supplied directory (relative to the
// session workspace); the runtime adds `<dir>/<sanitized_field>.<ext>`
// for each part. An empty `requestedDir` defaults to
// `data/<query_id>/`.
func (d *queryDeps) writeResponse(sessionID, requestedDir, queryID string, resp *types.Response) ([]partEntry, error) {
	if resp == nil || resp.Data == nil {
		return nil, nil
	}
	tables := resp.Tables()
	objects := resp.Objects()
	hasExt := len(resp.Extensions) > 0

	totalParts := len(tables) + len(objects)
	if hasExt {
		totalParts++
	}
	if totalParts == 0 {
		return nil, nil
	}

	dir, err := d.resolveOutDir(sessionID, requestedDir, queryID)
	if err != nil {
		return nil, err
	}

	out := make([]partEntry, 0, totalParts)

	for _, p := range tables {
		tbl, err := resp.Table(p)
		if errors.Is(err, types.ErrNoData) {
			out = append(out, partEntry{Part: p, Null: true})
			continue
		}
		if err != nil {
			return out, fmt.Errorf("table %s: %w", p, err)
		}
		if tbl == nil {
			out = append(out, partEntry{Part: p, Null: true})
			continue
		}
		path := filepath.Join(dir, sanitizeKey(p)+".parquet")
		rows, schema, err := writeParquet(path, tbl)
		if err != nil {
			return out, &toolError{Code: "io", Msg: err.Error()}
		}
		if rows == 0 {
			// Empty result — drop the placeholder file so the
			// model never sees a 0-byte leftover, and surface
			// `null` instead so the absence is explicit.
			_ = os.Remove(path)
			out = append(out, partEntry{Part: p, Null: true, Schema: schema})
			continue
		}
		size, _ := fileSize(path)
		out = append(out, partEntry{
			Path:     path,
			Format:   "parquet",
			Part:    p,
			Size:     size,
			RowCount: rows,
			Schema:   schema,
		})
	}

	for _, p := range objects {
		val := resp.DataPart(p)
		// The IPC client encodes JSON `null` as untyped Go nil
		// (client.go:557) and every other object part as
		// *types.JsonValue (client.go:563), which implements
		// json.Marshaler — json.Marshal therefore returns the raw
		// bytes verbatim with no re-encode. We just need the nil
		// guard before marshalling.
		if val == nil {
			out = append(out, partEntry{Part: p, Null: true})
			continue
		}
		body, err := json.Marshal(val)
		if err != nil {
			return out, &toolError{Code: "io", Msg: "marshal object " + p + ": " + err.Error()}
		}
		path := filepath.Join(dir, sanitizeKey(p)+".json")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return out, &toolError{Code: "io", Msg: err.Error()}
		}
		preview, truncated := jsonTextPreview(body)
		out = append(out, partEntry{
			Path:      path,
			Format:    "json",
			Part:     p,
			Size:      int64(len(body)),
			Preview:   preview,
			Truncated: truncated,
		})
	}

	if hasExt {
		body, err := json.Marshal(resp.Extensions)
		if err != nil {
			return out, &toolError{Code: "io", Msg: "marshal extensions: " + err.Error()}
		}
		path := filepath.Join(dir, "extensions.json")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return out, &toolError{Code: "io", Msg: err.Error()}
		}
		preview, truncated := jsonTextPreview(body)
		out = append(out, partEntry{
			Path:      path,
			Format:    "json",
			Part:     "extensions",
			Size:      int64(len(body)),
			Preview:   preview,
			Truncated: truncated,
		})
	}

	return out, nil
}

// fileSize returns the on-disk size in bytes. The error is ignored
// by callers — a missing or unstatable file just yields 0, which
// json-omitempty hides.
func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// jsonTextPreview returns a short textual snippet of `body`. The
// body is written verbatim up to jsonPreviewCap; longer payloads
// are truncated and Truncated=true is returned.
func jsonTextPreview(body []byte) (string, bool) {
	if len(body) <= jsonPreviewCap {
		return string(body), false
	}
	return string(body[:jsonPreviewCap]), true
}

// sanitizeKey turns a dotted GraphQL field path into a
// filesystem-safe filename stem. Dots become underscores so the
// resulting name doesn't masquerade as a nested filename; slashes
// and spaces are replaced too.
func sanitizeKey(k string) string {
	repl := strings.NewReplacer("/", "_", "\\", "_", " ", "_", ".", "_")
	return repl.Replace(k)
}

// writeParquet streams an ArrowTable to a Parquet file. Returns
// the row count and the Arrow schema rendered as []schemaColumn
// (column name + Arrow type string + per-field metadata when
// present). The schema is captured before the writer is closed,
// so it survives the streaming release.
//
// Atomic-write protocol: data goes to `<path>.tmp` first, then
// renames over `path` on success. On any failure the cleanup
// defer drops the tmp file — the target path is never half-
// written and a failed call leaves no stray bytes on disk.
func writeParquet(path string, tbl types.ArrowTable) (rows int, schema []schemaColumn, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, nil, err
	}
	reader, err := tbl.Reader(false)
	if err != nil {
		return 0, nil, fmt.Errorf("arrow reader: %w", err)
	}
	defer reader.Release()
	if reader == nil {
		return 0, nil, nil
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, nil, err
	}
	// pqarrow's Close flushes the footer and (via TellWrapper.Close
	// → io.WriteCloser type assert) ends up closing `f` too, so the
	// deferred f.Close is a belt-and-braces no-op on the success
	// path: a second Close on *os.File returns "file already
	// closed", which we ignore. On the early-error path (writer
	// construction fails) the defer is the only thing keeping the
	// fd from leaking.
	defer f.Close()

	arrowSchema := reader.Schema()
	schema = arrowSchemaToColumns(arrowSchema)

	w, err := pqarrow.NewFileWriter(arrowSchema, f, parquet.NewWriterProperties(), pqarrow.DefaultWriterProps())
	if err != nil {
		_ = os.Remove(tmp)
		return 0, nil, fmt.Errorf("parquet writer: %w", err)
	}
	// GeoParquet 1.1 file-level metadata. Without this DuckDB
	// (and every other GeoParquet reader) treats geometry columns
	// as plain BLOB, even though the field-level ARROW:extension
	// metadata is preserved by pqarrow. arrow-go does not emit it
	// itself — there's no GeoParquet awareness anywhere in the
	// library.
	if geo := buildGeoFileMetadata(arrowSchema); geo != "" {
		if err := w.AppendKeyValueMetadata("geo", geo); err != nil {
			_ = w.Close()
			_ = os.Remove(tmp)
			return 0, nil, fmt.Errorf("parquet geo metadata: %w", err)
		}
	}
	for reader.Next() {
		batch := reader.RecordBatch()
		if err := w.WriteBuffered(batch); err != nil {
			_ = w.Close()
			_ = os.Remove(tmp)
			return 0, nil, fmt.Errorf("parquet write: %w", err)
		}
		rows += int(batch.NumRows())
	}
	if err := w.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, nil, fmt.Errorf("parquet close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, nil, err
	}
	return rows, schema, nil
}

// arrowSchemaToColumns flattens an arrow.Schema into the result
// envelope's []schemaColumn shape. Per-field metadata is exposed
// only when the field carries any (the common case is empty).
func arrowSchemaToColumns(s *arrow.Schema) []schemaColumn {
	if s == nil {
		return nil
	}
	fields := s.Fields()
	out := make([]schemaColumn, 0, len(fields))
	for _, f := range fields {
		col := schemaColumn{
			Name: f.Name,
			Type: f.Type.String(),
		}
		if md := f.Metadata; md.Len() > 0 {
			meta := make(map[string]string, md.Len())
			keys := md.Keys()
			values := md.Values()
			for i := range keys {
				meta[keys[i]] = values[i]
			}
			col.Metadata = meta
		}
		out = append(out, col)
	}
	return out
}
