# kennyscope

A small observer that watches Kenny's container from outside, parses his stdout/stderr, and serves a per-life timeline at an HTTP address of your choosing.

Kenny is unaware of kennyscope. Nothing in Kenny's repo references it. kennyscope reads Kenny's container logs via the Docker Engine API through a `tecnativa/docker-socket-proxy` sibling — read-only access to `/containers` and `/containers/*/logs` only.

## Data flow

```
Kenny container stdout/stderr
        │
        ▼
 Docker engine (host)
        │  (read-only /containers, /logs)
        ▼
 docker-socket-proxy  ← least-privilege API proxy
        │  HTTP :2375
        ▼
 kennyscope.tailer   ← parses slog JSON, groups by life_id
        │
        ▼
 kennyscope.store    ← SQLite at /state/observer.db
        │
        ▼
 kennyscope.web      ← server-rendered HTML, :8080
```

## Pages

- `GET /` — list of lives, newest first. Link into per-life detail.
- `GET /lives/{id}` — full event stream for one life (stdout + stderr, timestamps, level, msg, pretty-printed JSON).
- `GET /healthz` — deep check: SQLite reachable. Returns 200 or 503.

## Environment variables

| Name | Default | Notes |
|------|---------|-------|
| `STATE_DIR` | `/state` | where `observer.db` lives |
| `HTTP_ADDR` | `:8080` | bind address |
| `DOCKER_HOST` | `http://docker-socket-proxy:2375` | Docker API endpoint |
| `KENNY_CONTAINER_MATCH` | `kenny` | substring match on container name |
| `OBSERVER_USER` | _(empty)_ | optional HTTP basic auth username |
| `OBSERVER_PASSWORD` | _(empty)_ | optional HTTP basic auth password |

If both `OBSERVER_USER` and `OBSERVER_PASSWORD` are set, `/` and `/lives/*` require basic auth. `/healthz` is always public.

## Running on Coolify

- Create a new application in the same Coolify project as Kenny (so the docker-socket-proxy that's bundled here can reach the host Docker daemon — which it always can via the mounted socket regardless of project isolation).
- **Build pack**: Docker Compose.
- **Source**: this repo.
- **Compose file**: `docker-compose.yml`.
- **Environment variables**:
  - `KENNY_CONTAINER_MATCH` — set to whatever substring identifies Kenny's container (check `docker ps` on the host; Coolify typically names it something like `kenny-kenny-abc123`).
  - `OBSERVER_USER` and `OBSERVER_PASSWORD` if you want basic auth. Recommended.
- Attach a domain to the `kennyscope` service at port `8080`.

## Local run

```
docker compose up --build
```

Then browse http://localhost:8080. Locally you'll need to override `KENNY_CONTAINER_MATCH` to whatever your local Kenny is named.

## Design notes

- **One-way visibility.** kennyscope reads; Kenny doesn't know. There is no Kenny-side configuration, no Kenny-side env var, no Kenny-side code path that knows about this service.
- **No double-writing.** Kenny keeps writing to stdout whether kennyscope is running or not. If kennyscope dies, Kenny's logs are still captured by Docker and Coolify's native viewer.
- **Resumable.** kennyscope persists a per-container cursor so a restart doesn't re-ingest history.
- **WAL-free.** kennyscope writes to its own SQLite and never reads Kenny's `/state` volume.
- **Failure modes.** If Kenny is missing, kennyscope logs a warning and retries. If docker-socket-proxy is down, same. If kennyscope's own SQLite is wedged, `/healthz` returns 503 and Coolify can auto-revert.
