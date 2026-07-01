# docker/ — dev test-infra

Containers for testing hugen locally. Not part of the runtime; dev-only.

## A2A Inspector — visual A2A client

The official [a2a-inspector](https://github.com/a2aproject/a2a-inspector) built
self-contained (it clones + builds upstream; we vendor nothing). Use it to
eyeball the hugen A2A endpoint: fetch the agent card, send messages, watch the
stream, trigger an `input-required` inquiry, receive a `FilePart` artifact.

### Run

```sh
# 1. Build + run the inspector (host port 10081 → container 8080).
docker compose -f docker/docker-compose.yml up -d --build

# 2. Boot hugen so the card advertises a container-reachable URL.
HUGEN_A2A_BASE_URL=http://host.docker.internal:10000 ./bin/hugen a2a
#   (add HUGEN_A2A_API_KEY=<secret> to gate the endpoint; then supply the
#    X-API-Key header in the inspector's request settings.)

# 3. Open the inspector and connect.
open http://127.0.0.1:10081
#   Agent URL: http://host.docker.internal:10000
```

The inspector fetches `…/.well-known/agent-card.json`, then talks to the card's
advertised interface URL (`http://host.docker.internal:10000/a2a`) — which is
why hugen must boot with `HUGEN_A2A_BASE_URL` set to that host.

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
