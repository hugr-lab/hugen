package hugenclient

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// StreamEvent is one item from Stream: a decoded frame with its resume cursor
// (Seq; 0 for streaming chunks). A terminal Err (then the channel closes)
// reports a stream failure.
type StreamEvent struct {
	Seq   int
	Frame protocol.Frame
	Err   error
}

// maxSSELine caps a single SSE line (a large consolidated frame + base64 could
// be big).
const maxSSELine = 8 << 20

// Stream opens the session's SSE frame stream. It replays from lastEventID (0 =
// from the start) then streams live. The returned channel closes when the
// context is cancelled or the stream ends; a terminal error arrives as a
// StreamEvent with Err set. Multiple Streams on one session are independent
// (the runtime fans out to each).
func (c *Client) Stream(ctx context.Context, id string, lastEventID int) (<-chan StreamEvent, error) {
	return c.stream(ctx, "/v1/sessions/"+url.PathEscape(id)+"/stream", lastEventID)
}

// StreamLive opens the stream in live-only mode (?live=1): no history replay,
// only frames from now. The A2A bridge uses this — it submits a fresh turn and
// wants ONLY that turn's frames, not the whole session history.
func (c *Client) StreamLive(ctx context.Context, id string) (<-chan StreamEvent, error) {
	return c.stream(ctx, "/v1/sessions/"+url.PathEscape(id)+"/stream?live=1", 0)
}

func (c *Client) stream(ctx context.Context, path string, lastEventID int) (<-chan StreamEvent, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID > 0 {
		req.Header.Set("Last-Event-ID", strconv.Itoa(lastEventID))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, errorFrom(resp)
	}

	out := make(chan StreamEvent)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)

		var curID int
		var data strings.Builder
		emit := func() {
			if data.Len() == 0 {
				return
			}
			frame, derr := c.codec.DecodeFrame([]byte(data.String()))
			data.Reset()
			if derr != nil {
				return // skip an undecodable event rather than kill the stream
			}
			if curID > 0 {
				if s, ok := frame.(protocol.SeqSetter); ok {
					s.SetSeq(curID)
				}
			}
			select {
			case out <- StreamEvent{Seq: curID, Frame: frame}:
			case <-ctx.Done():
			}
		}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "": // event boundary
				emit()
				curID = 0
			case strings.HasPrefix(line, ":"): // comment / heartbeat
			case strings.HasPrefix(line, "id:"):
				curID, _ = strconv.Atoi(strings.TrimSpace(line[len("id:"):]))
			case strings.HasPrefix(line, "data:"):
				// json.Marshal never emits raw newlines, so the server sends the
				// whole frame on one data line — a plain trim suffices.
				data.WriteString(strings.TrimSpace(line[len("data:"):]))
			}
		}
		emit() // flush a trailing event with no blank line
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			select {
			case out <- StreamEvent{Err: err}:
			default:
			}
		}
	}()
	return out, nil
}
