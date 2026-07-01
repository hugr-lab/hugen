# hugen native HTTP API — dev runbook (H9)

Hands-on recipes for the native HTTP API (`hugen serve`,
`design/008-integration/spec-http-api.md`). Dev uses **allow-open** (no token);
a real deployment forwards the user's hub token as `Authorization: Bearer …`.

```sh
# Boot the API (dev): auth listener stays on :10000 (IdP redirect), API on :10100.
HUGEN_API_PORT=10100 HUGEN_API_ALLOW_OPEN=1 ./bin/hugen serve
API=http://localhost:10100
```

## The ladder

### 1. curl

```sh
# agent card + identity
curl -s $API/v1/agent | jq
curl -s $API/v1/whoami | jq                       # {user_id:"local",…} in allow-open

# create a session
SID=$(curl -s -X POST $API/v1/sessions -d '{"name":"demo"}' | jq -r .session_id)

# stream it (leave running in another shell) — SSE, id: = resume cursor
curl -N $API/v1/sessions/$SID/stream

# send a message (reply arrives on the stream)
curl -s -X POST $API/v1/sessions/$SID/messages -d '{"text":"What tables are in the catalog?"}'

# answer a HITL inquiry (request_id from the inquiry_request frame on the stream)
curl -s -X POST $API/v1/sessions/$SID/inquiry -d '{"request_id":"<id>","response":"EMEA"}'

# cancel the in-flight turn
curl -s -X POST $API/v1/sessions/$SID/cancel -d '{"cascade":true}'

# history + artifacts
curl -s "$API/v1/sessions/$SID/events?from=0&limit=50" | jq '.[].event_type'
curl -s $API/v1/sessions/$SID/artifacts | jq
curl -s -X POST "$API/v1/sessions/$SID/artifacts?name=note.txt" --data-binary "hi" | jq
curl -s $API/v1/sessions/$SID/artifacts/note.txt

# my sessions / one / close
curl -s $API/v1/sessions | jq '.[].id'
curl -s -X DELETE $API/v1/sessions/$SID

# (keyed deployment: add  -H "Authorization: Bearer $USER_TOKEN"  to every call)
```

### 2. Go client (`pkg/hugenclient`)

```go
c := hugenclient.New("http://localhost:10100" /*, hugenclient.WithToken(tok)*/)
id, _ := c.CreateSession(ctx, hugenclient.CreateSessionOptions{Name: "demo"})
ch, _ := c.StreamLive(ctx, id)          // or Stream(ctx, id, lastEventID) to replay
_ = c.SendMessage(ctx, id, "hello")
for ev := range ch { /* ev.Frame, ev.Seq */ }
```

Run the guarded integration test against a live `hugen serve`:

```sh
HUGEN_API_URL=http://localhost:10100 go test ./pkg/hugenclient/ -run Integration -v
```

### 3. Browser — the multi-interface proof

Open the built-in dev client and **prove many interfaces drive one session**.
It is OFF by default — enable it with `HUGEN_API_DEV_UI=1` on the `hugen serve`
run (unauthenticated; dev only):

```sh
HUGEN_API_PORT=10100 HUGEN_API_ALLOW_OPEN=1 HUGEN_API_DEV_UI=1 ./bin/hugen serve
open http://localhost:10100/ui
```

Send a message, then click **copy 2-tab URL** and open it in a **second tab** —
both tabs stream the same session live (the runtime's multi-subscriber fanout).
The dev client is `/ui`, allow-open only (EventSource can't send an auth header);
the real hub UI is an external app on this same API.
