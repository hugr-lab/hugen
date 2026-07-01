package hugenclient

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Session is the owner-facing view of a session.
type Session struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	OpenedAt  time.Time `json:"opened_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateSessionOptions parameterises CreateSession (all optional).
type CreateSessionOptions struct {
	Name     string         `json:"name,omitempty"`
	Tier     string         `json:"tier,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// CreateSession opens a root session and returns its id.
func (c *Client) CreateSession(ctx context.Context, opts CreateSessionOptions) (string, error) {
	var resp struct {
		SessionID string `json:"session_id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sessions", opts, &resp); err != nil {
		return "", err
	}
	return resp.SessionID, nil
}

// ListSessions returns the caller's sessions, optionally filtered by status.
func (c *Client) ListSessions(ctx context.Context, status string) ([]Session, error) {
	path := "/v1/sessions"
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}
	var out []Session
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// GetSession returns one owned session.
func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	var s Session
	err := c.doJSON(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id), nil, &s)
	return s, err
}

// CloseSession terminates a session.
func (c *Client) CloseSession(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(id), nil, nil)
}

// SendMessage submits a user message. The reply arrives on Stream.
func (c *Client) SendMessage(ctx context.Context, id, text string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/messages",
		map[string]string{"text": text}, nil)
}

// InquiryAnswer answers a pending HITL inquiry.
type InquiryAnswer struct {
	RequestID        string                          `json:"request_id"`
	Approved         *bool                           `json:"approved,omitempty"`
	Response         string                          `json:"response,omitempty"`
	Reason           string                          `json:"reason,omitempty"`
	Answers          map[string]protocol.AnswerEntry `json:"answers,omitempty"`
	AutoApproveTools bool                            `json:"auto_approve_tools,omitempty"`
}

// AnswerInquiry submits an inquiry response.
func (c *Client) AnswerInquiry(ctx context.Context, id string, ans InquiryAnswer) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/inquiry", ans, nil)
}

// Cancel aborts the session's in-flight turn (cascade also stops sub-agents).
func (c *Client) Cancel(ctx context.Context, id string, cascade bool) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/cancel",
		map[string]bool{"cascade": cascade}, nil)
}
