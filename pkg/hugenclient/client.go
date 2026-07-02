// Package hugenclient is a typed Go client for hugen's native HTTP API
// (design/008-integration/spec-http-api.md). It mirrors the AdapterHost surface
// over HTTP + SSE: create/list/close sessions, submit messages / inquiry
// answers / cancels, stream frames, and read history + artifacts.
//
// It is dependency-light — stdlib + pkg/protocol only — so external consumers
// (the A2A bridge in cmd/a2a, a hub UI backend, tests) can import it without
// pulling the runtime. H7.
package hugenclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Client talks to one hugen HTTP API endpoint.
type Client struct {
	base  string
	token string
	http  *http.Client
	codec *protocol.Codec
}

// Option configures a Client.
type Option func(*Client)

// WithToken sets the bearer token sent on every request (the user's forwarded
// hub token; empty in allow-open dev).
func WithToken(t string) Option { return func(c *Client) { c.token = t } }

// WithHTTPClient overrides the underlying http.Client (e.g. custom timeout /
// transport). The default has no timeout — streams are long-lived.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New constructs a client for baseURL (e.g. "http://localhost:10100").
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		base:  trimSlash(baseURL),
		http:  &http.Client{},
		codec: protocol.NewCodec(),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("hugenclient: http %d", e.Status)
	}
	return fmt.Sprintf("hugenclient: http %d: %s", e.Status, e.Message)
}

// newRequest builds a request with the bearer header set.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// doJSON sends reqBody (JSON, or nil) and decodes the response into respOut (or
// nil to discard). Non-2xx → *APIError.
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respOut any) error {
	var r io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, r)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return errorFrom(resp)
	}
	if respOut != nil {
		return json.NewDecoder(resp.Body).Decode(respOut)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// decodeJSON decodes r into v.
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

// errorFrom reads the {"error": "..."} body into an *APIError.
func errorFrom(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	return &APIError{Status: resp.StatusCode, Message: body.Error}
}

// User is the verified identity from GET /v1/whoami.
type User struct {
	UserID string `json:"user_id"`
	Name   string `json:"name,omitempty"`
	Role   string `json:"role,omitempty"`
}

// WhoAmI returns the identity the agent resolved for the client's token.
func (c *Client) WhoAmI(ctx context.Context) (User, error) {
	var u User
	err := c.doJSON(ctx, http.MethodGet, "/v1/whoami", nil, &u)
	return u, err
}
