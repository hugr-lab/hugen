# Self-contained build of the official A2A Inspector
# (github.com/a2aproject/a2a-inspector) — a visual A2A client used to eyeball
# the hugen A2A endpoint (card, turns, streaming, input-required inquiries,
# FilePart artifacts). It clones + builds the upstream inspector; we vendor
# nothing. Mirrors the upstream multi-stage build, but pulls the source in a
# single `src` stage so `docker build` works from anywhere with no local clone.
#
#   docker build -t a2a-inspector -f docker/a2a-inspector.Dockerfile docker/
#   docker run -d --name a2a-inspector -p 10081:8080 \
#       --add-host=host.docker.internal:host-gateway a2a-inspector
#   # open http://127.0.0.1:10081 → connect to http://host.docker.internal:10000
#
# The container listens on 8080; expose it on a 10000-range host port (10081).

# Stage 0: fetch the upstream source once.
FROM alpine/git AS src
RUN git clone --depth 1 https://github.com/a2aproject/a2a-inspector.git /src

# Stage 1: build the frontend assets (upstream outputs to /app/public).
FROM node:18-alpine AS frontend-builder
WORKDIR /app
COPY --from=src /src/frontend/ ./
RUN npm ci && npm rebuild esbuild && npm run build

# Stage 2: backend + built frontend.
FROM python:3.12-slim
WORKDIR /app
RUN pip install uv
COPY --from=src /src/pyproject.toml /src/uv.lock ./
RUN uv sync --no-cache && uv pip install validators
COPY --from=src /src/backend ./backend
RUN mkdir -p /app/frontend
COPY --from=frontend-builder /app/public /app/frontend/public

WORKDIR /app/backend
EXPOSE 8080
CMD ["uv", "run", "--", "uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8080"]
