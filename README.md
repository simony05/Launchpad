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
├── internal/httpserver/     # HTTP routes and server construction
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

## Local development

Go 1.24 or later is required.

```sh
go test ./...
go run ./cmd/control-plane
```

In another terminal, test the service:

```sh
curl -i http://localhost:8080/health
```

Expected response:

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"status":"ok"}
```

To set configuration explicitly:

```sh
MINICLOUD_HOST=127.0.0.1 MINICLOUD_PORT=8081 MINICLOUD_LOG_LEVEL=debug go run ./cmd/control-plane
curl -i http://localhost:8081/health
```

## Docker

Build and run the control plane:

```sh
docker build -t minicloud-control-plane .
docker run --rm -p 8080:8080 minicloud-control-plane
```

Then use the same `curl` command above. Pass environment configuration with
`-e`, for example `-e MINICLOUD_LOG_LEVEL=debug`.
