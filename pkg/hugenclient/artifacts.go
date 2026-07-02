package hugenclient

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Event is one persisted event-log row (matches the /events JSON).
type Event struct {
	Seq        int            `json:"seq"`
	EventType  string         `json:"event_type"`
	Author     string         `json:"author,omitempty"`
	Content    string         `json:"content,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolArgs   map[string]any `json:"tool_args,omitempty"`
	ToolResult string         `json:"tool_result,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at,omitempty"`
}

// EventsOptions filters the history query.
type EventsOptions struct {
	From  int      // resume cursor (seq); events with seq > From
	Limit int      // 0 ⇒ server default
	Kinds []string // event-type filter
}

// Events returns the session's persisted event log (history / scroll-back).
func (c *Client) Events(ctx context.Context, id string, opts EventsOptions) ([]Event, error) {
	q := url.Values{}
	if opts.From > 0 {
		q.Set("from", strconv.Itoa(opts.From))
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if len(opts.Kinds) > 0 {
		q.Set("kinds", strings.Join(opts.Kinds, ","))
	}
	path := "/v1/sessions/" + url.PathEscape(id) + "/events"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out []Event
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// ListArtifacts lists the session's artifacts.
func (c *Client) ListArtifacts(ctx context.Context, id string) ([]protocol.ArtifactRef, error) {
	var out []protocol.ArtifactRef
	err := c.doJSON(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id)+"/artifacts", nil, &out)
	return out, err
}

// DownloadArtifact streams an artifact's bytes. The caller MUST close the
// returned reader.
func (c *Client) DownloadArtifact(ctx context.Context, id, artifactID string) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, http.MethodGet,
		"/v1/sessions/"+url.PathEscape(id)+"/artifacts/"+url.PathEscape(artifactID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, errorFrom(resp)
	}
	return resp.Body, nil
}

// IngestArtifact uploads r into the session's artifact scope.
func (c *Client) IngestArtifact(ctx context.Context, id, name string, r io.Reader) (protocol.ArtifactRef, error) {
	path := "/v1/sessions/" + url.PathEscape(id) + "/artifacts?name=" + url.QueryEscape(name)
	req, err := c.newRequest(ctx, http.MethodPost, path, r)
	if err != nil {
		return protocol.ArtifactRef{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return protocol.ArtifactRef{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return protocol.ArtifactRef{}, errorFrom(resp)
	}
	var ref protocol.ArtifactRef
	if err := decodeJSON(resp.Body, &ref); err != nil {
		return protocol.ArtifactRef{}, err
	}
	return ref, nil
}
