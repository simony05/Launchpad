# MiniCloud

MiniCloud is an agent-native deployment runtime for AI-generated prototypes.

This repository currently contains the MiniCloud control plane's first
milestone: a small Go HTTP service with a health endpoint. It intentionally
does not yet build or run user applications, schedule work, communicate with
workers, or route public prototype traffic.

## Project structure

```text
.
├── cmd/control-plane/       # Process entrypoint and shutdown lifecycle
├── internal/config/         # Environment-derived, validated application config
├── internal/database/       # PostgreSQL connection and embedded SQL migrations
├── internal/deployments/    # Deployment domain model and persistence repository
├── internal/httpserver/     # HTTP routes and server construction
├── internal/workspace/      # Validated per-deployment source storage
├── Dockerfile               # Container image for the control-plane binary
└── go.mod                   # Go module definition
```

As MiniCloud grows, deployment, worker, scheduler, build, and routing
packages can be added under `internal/` without exposing implementation
details as public Go APIs.

## Configuration

| Environment variable | Default | Description |
| --- | --- | --- |
| `MINICLOUD_HOST` | `0.0.0.0` | Address on which the server listens. |
| `MINICLOUD_PORT` | `8080` | TCP port on which the server listens. |
| `MINICLOUD_LOG_LEVEL` | `info` | Structured log level: `debug`, `info`, `warn`, or `error`. |
| `MINICLOUD_DATABASE_URL` | none | Required PostgreSQL connection URL. |
| `MINICLOUD_WORKSPACE_ROOT` | none | Required absolute path for deployment source workspaces. |

## Deployment metadata API

`POST /deployments` creates metadata and stores a generated Python source
bundle. It does not build, start, or otherwise execute application code.

```json
{
  "name": "example-api",
  "runtime": "python",
  "files": {
    "app.py": "from fastapi import FastAPI\napp = FastAPI()\n",
    "requirements.txt": "fastapi\nuvicorn\n"
  }
}
```

The response is `201 Created` and has a `PENDING` status. V1 accepts exactly
`app.py` (up to 1 MiB) and `requirements.txt` (up to 64 KiB). Other filenames,
including path-like names, are rejected. Read metadata with `GET /deployments/{id}`
or list it with `GET /deployments`.

The initial migration creates a `deployments` table with UUID identifiers,
database-managed timestamps, and a PostgreSQL `deployment_status` enum. The
runtime is currently restricted to `python`; nullable container, port, and
public-identifier fields are reserved for future worker and routing milestones.

## Development database

Create an isolated Docker network and a persistent PostgreSQL container:

```sh
docker network create minicloud
docker volume create minicloud-postgres-data
docker volume create minicloud-workspaces
docker run -d --name minicloud-postgres --restart unless-stopped \
  --network minicloud \
  -e POSTGRES_DB=minicloud \
  -e POSTGRES_USER=minicloud \
  -e POSTGRES_PASSWORD=replace-this-development-password \
  -v minicloud-postgres-data:/var/lib/postgresql/data \
  postgres:17
```

The database has no published host port. A control-plane container on the same
Docker network reaches it using the `minicloud-postgres` hostname.

## Local development

Go 1.24 or later is required.

```sh
docker run --rm --name minicloud-postgres -p 5432:5432 \
  -e POSTGRES_DB=minicloud \
  -e POSTGRES_USER=minicloud \
  -e POSTGRES_PASSWORD=replace-this-development-password \
  postgres:17
```

In another terminal, run the service and test it:

```sh
MINICLOUD_DATABASE_URL='postgres://minicloud:replace-this-development-password@localhost:5432/minicloud?sslmode=disable' MINICLOUD_WORKSPACE_ROOT=/tmp/minicloud-workspaces go run ./cmd/control-plane
curl -i http://localhost:8080/health
curl -i -X POST http://localhost:8080/deployments \
  -H 'Content-Type: application/json' \
  -d '{"name":"example-api","runtime":"python","files":{"app.py":"from fastapi import FastAPI","requirements.txt":"fastapi"}}'
```

Expected response:

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"status":"ok"}
```

To set configuration explicitly:

```sh
MINICLOUD_HOST=127.0.0.1 MINICLOUD_PORT=8081 MINICLOUD_LOG_LEVEL=debug MINICLOUD_DATABASE_URL='postgres://minicloud:replace-this-development-password@localhost:5432/minicloud?sslmode=disable' MINICLOUD_WORKSPACE_ROOT=/tmp/minicloud-workspaces go run ./cmd/control-plane
curl -i http://localhost:8081/health
```

## Docker

Build and run the control plane:

```sh
docker build -t minicloud-control-plane .
docker run --rm --network minicloud -p 8080:8080 \
  -v minicloud-workspaces:/workspaces \
  -e MINICLOUD_DATABASE_URL='postgres://minicloud:replace-this-development-password@minicloud-postgres:5432/minicloud?sslmode=disable' \
  -e MINICLOUD_WORKSPACE_ROOT=/workspaces \
  minicloud-control-plane
```

Then use the same `curl` command above. Pass environment configuration with
`-e`, for example `-e MINICLOUD_LOG_LEVEL=debug`.
