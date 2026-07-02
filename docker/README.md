# docker/ — dev test-infra

Containers for testing hugen locally. Not part of the runtime; dev-only.

## A2A Inspector — visual A2A client

The official [a2a-inspector](https://github.com/a2aproject/a2a-inspector) built
self-contained (it clones + builds upstream; we vendor nothing). Use it to
eyeball the hugen A2A endpoint: fetch the agent card, send messages, watch the
stream, trigger an `input-required` inquiry, receive a `FilePart` artifact.

### Run

Since H8 the A2A surface is a STANDALONE bridge (`bin/a2a`) that drives hugen
through the native HTTP API (`hugen serve`). Two processes:

```sh
# 1. Build + run the inspector (host port 10081 → container 8080).
docker compose -f docker/docker-compose.yml up -d --build

# 2. Boot the native HTTP API. auth listener stays on :10000 (the IdP redirect
#    URI is pinned there); the API is on :10100. Dev = allow-open.
HUGEN_API_PORT=10100 HUGEN_API_ALLOW_OPEN=1 ./bin/hugen serve

# 3. Boot the A2A bridge, pointing it at the API. It serves A2A on :10010 and
#    advertises a container-reachable card URL.
HUGEN_API_URL=http://localhost:10100 \
  HUGEN_A2A_PORT=10010 \
  HUGEN_A2A_BASE_URL=http://host.docker.internal:10010 \
  HUGEN_A2A_ALLOW_OPEN=1 ./bin/a2a
#   (gate it: HUGEN_A2A_API_KEY=<secret>, drop HUGEN_A2A_ALLOW_OPEN;
#    hub run: HUGEN_API_TOKEN=<user-token> so sessions are owned per-user.)

# 4. Open the inspector and connect.
open http://127.0.0.1:10081
#   Agent URL: http://host.docker.internal:10010
```

The inspector fetches `…/.well-known/agent-card.json` from the bridge, then
talks to the card's advertised interface URL
(`http://host.docker.internal:10010/a2a`) — which is why the bridge must run
with `HUGEN_A2A_BASE_URL` set to that host. The bridge in turn drives hugen at
`HUGEN_API_URL`.

### Stop

```sh
docker compose -f docker/docker-compose.yml down
```

### Plain docker (no compose)

```sh
docker build -t a2a-inspector -f docker/a2a-inspector.Dockerfile docker/
docker run -d --name a2a-inspector -p 10081:8080 \
    --add-host=host.docker.internal:host-gateway a2a-inspector
```
