# Launchpad

Milestone 15: [scale idle prototypes to zero](docs/scale-to-zero.md) adds retained
container sleep/wake, request activity tracking, readiness waits, and cold-start
latency logging. Enable it after upgrading both workers; it defaults to off.

Milestone 14: [worker failure recovery](docs/worker-failover.md) adds durable source,
grace periods, confirmed EC2 fencing, and versioned rescheduling. Automatic worker
recovery is opt-in and requires worker identities/tags and scoped EC2 permissions.

Milestone 13: [application health and bounded crash recovery](docs/application-health.md)
adds opt-in HTTP health checks, durable restart limits and runtime diagnostics.

Milestone 12: [private cross-worker routing](docs/private-routing.md) adds live
route verification and requires `LAUNCHPAD_APP_BIND_IP` on each worker.

Milestone 11: see [multi-worker scheduling](docs/multi-worker.md) for the current
control-plane plus two-worker configuration. See also
[worker registration and heartbeats](docs/worker-heartbeats.md).
The earlier single-host commands
below are historical; the control plane must not mount the Docker socket.

Launchpad is an agent-native deployment runtime for AI-generated prototypes.

This repository contains the Launchpad control plane. It accepts a small,
validated Python/FastAPI source bundle, builds a Docker image from a
Launchpad-owned runtime template, and starts the resulting container. It does
not schedule work or communicate with workers yet. Applications are available
through a single-host reverse proxy and HTTPS edge.

## Project structure

```text
.
├── cmd/control-plane/       # Process entrypoint and shutdown lifecycle
├── internal/config/         # Environment-derived, validated application config
├── internal/database/       # PostgreSQL connection and embedded SQL migrations
├── internal/deployments/    # Deployment domain model and persistence repository
├── internal/build/          # Launchpad-owned Docker image builder
├── internal/containers/     # Docker lifecycle for generated applications
├── internal/httpserver/     # HTTP routes and server construction
├── internal/routing/        # Public application reverse proxy and location cache
├── Caddyfile                # Public HTTPS termination configuration
├── internal/workspace/      # Validated per-deployment source storage
├── Dockerfile               # Container image for the control-plane binary
└── go.mod                   # Go module definition
```

As Launchpad grows, deployment, worker, scheduler, build, and routing
packages can be added under `internal/` without exposing implementation
details as public Go APIs.

## Configuration

| Environment variable | Default | Description |
| --- | --- | --- |
| `LAUNCHPAD_HOST` | `0.0.0.0` | Address on which the server listens. |
| `LAUNCHPAD_PORT` | `8080` | TCP port on which the server listens. |
| `LAUNCHPAD_LOG_LEVEL` | `info` | Structured log level: `debug`, `info`, `warn`, or `error`. |
| `LAUNCHPAD_DATABASE_URL` | none | Required PostgreSQL connection URL. |
| `LAUNCHPAD_WORKSPACE_ROOT` | none | Required absolute path for deployment source workspaces. |
| `LAUNCHPAD_BUILD_TIMEOUT_SECONDS` | `300` | Docker build deadline, from 30 to 1,800 seconds. |
| `LAUNCHPAD_START_TIMEOUT_SECONDS` | `30` | Docker container-start deadline, from 5 to 300 seconds. |
| `LAUNCHPAD_APP_CPUS` | `0.5` | CPU limit passed to each generated application container. |
| `LAUNCHPAD_APP_MEMORY` | `256m` | Memory limit passed to each generated application container. |
| `LAUNCHPAD_ROUTER_UPSTREAM_HOST` | `host.docker.internal` | Docker-host address used by the router to reach published application ports. |
| `LAUNCHPAD_ROUTER_CACHE_TTL_SECONDS` | `5` | Application location-cache lifetime, from 1 to 60 seconds. |
| `LAUNCHPAD_PUBLIC_BASE_DOMAIN` | none | Required public base domain, such as `apps.example.com`. |

## Deployment metadata API

`POST /deployments` creates metadata, stores a generated Python source bundle,
builds an image, and starts the resulting application container.

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

The response is `201 Created`. V1 accepts exactly
`app.py` (up to 1 MiB) and `requirements.txt` (up to 64 KiB). Other filenames,
including path-like names, are rejected. Read metadata with `GET /deployments/{id}`
or list it with `GET /deployments`.

The control plane transitions a valid deployment from `PENDING` to `BUILDING`,
then `READY_TO_START`, `STARTING`, and finally `RUNNING`. Build or startup
errors transition to `FAILED`. The response stores the deterministic image
name `launchpad/deployment:<deployment-id>-v1`, bounded build output, and
short build/start errors when applicable.

Launchpad generates a fixed Python 3.13 runtime template that installs
`requirements.txt` and runs `uvicorn app:app` on port 8000. Agents cannot
supply a Dockerfile.

When started, Docker publishes the container's port 8000 on an automatically
selected free EC2 host port. A running deployment response contains
`container_id`, `internal_port` (`8000`), and `host_port`. Access it directly
with `http://<EC2-public-ip>:<host_port>` for diagnostics; use the routed path
below for normal access.

`DELETE /deployments/{id}` removes a running container and retains the
deployment record with `STOPPED` status, preserving its build metadata.

## Application routing

Each deployment has a Launchpad-generated, URL-safe `public_identifier`. A
running application is available at:

```text
https://<public_identifier>.<public-base-domain>/...
```

The control-plane response includes `public_url` only after the deployment is
`RUNNING`. Its Host-header router extracts the left-most label, resolves that
identifier to a deployment, caches its host port briefly, and proxies the full
path. Method, path, query, body, and normal headers are preserved. Unknown
identifiers return `404`; a known but non-running deployment returns `503`; an
unreachable application port returns `502`.

## Public HTTPS

Launchpad uses Caddy as a small edge proxy. Caddy terminates TLS on ports 80
and 443, retains the original Host header, and forwards HTTP over the private
Docker network to Launchpad. Launchpad then selects the generated application.
TLS does not terminate in individual generated application containers.

This configuration uses wildcard DNS with Caddy on-demand TLS: Caddy issues
and renews an ACME certificate for each active prototype hostname on its first
HTTPS request. This avoids a DNS-provider-specific Caddy build and a wildcard
certificate; the control-plane `ask` endpoint authorizes only hostnames for
currently running deployments.

### EC2 setup

Choose a base domain such as `apps.example.com`. At your DNS provider, create
an `A` record:

```text
*.apps.example.com  ->  <EC2 Elastic IP or public IPv4>
```

Use an Elastic IP if you need the DNS destination to remain stable across an
instance stop/start. In the EC2 security group, add inbound TCP rules for ports
80 and 443 from `0.0.0.0/0`. Keep the control-plane port 8080 limited to your
own IP; Caddy reaches it privately over Docker networking.

Build and run the control plane with its public domain configured. Replace the
database password and domain values with yours:

```sh
docker build -t launchpad-control-plane:milestone-8 .

docker rm -f launchpad-control-plane
docker run -d --name launchpad-control-plane --restart unless-stopped \
  --network launchpad \
  -p 8080:8080 \
  -v launchpad-workspaces:/workspaces \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --add-host=host.docker.internal:host-gateway \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -e LAUNCHPAD_DATABASE_URL='postgres://launchpad:YOUR_POSTGRES_PASSWORD@launchpad-postgres:5432/launchpad?sslmode=disable' \
  -e LAUNCHPAD_WORKSPACE_ROOT=/workspaces \
  -e LAUNCHPAD_ROUTER_UPSTREAM_HOST=host.docker.internal \
  -e LAUNCHPAD_PUBLIC_BASE_DOMAIN=apps.example.com \
  launchpad-control-plane:milestone-8
```

Start Caddy on the same Docker network. Its data volume persists issued
certificates and renewal state:

```sh
docker volume create launchpad-caddy-data

docker run -d --name launchpad-caddy --restart unless-stopped \
  --network launchpad \
  -p 80:80 \
  -p 443:443 \
  -v launchpad-caddy-data:/data \
  -v "$(pwd)/Caddyfile:/etc/caddy/Caddyfile:ro" \
  -e LAUNCHPAD_ACME_EMAIL=you@example.com \
  caddy:2
```

After DNS has propagated, create a deployment and open its returned
`public_url`. The first HTTPS request can take a few seconds while Caddy obtains
the certificate; subsequent requests use the stored certificate.

## Build security

The build context contains only the two validated source files plus a
temporary Dockerfile generated by Launchpad. Docker is invoked from Go with an
argument list, not a shell command, and the image tag contains only a UUID and
version. This blocks obvious filename, path traversal, and shell-injection
abuse.

It does not make builds safe for hostile multi-tenant code. `pip install` can
execute package build hooks, and mounting the Docker socket gives the control
plane effective root-level authority on its host. Keep this single-host setup
restricted to trusted development use. A future worker isolation milestone
must address that boundary before untrusted public workloads are supported.

Generated containers use Docker's default bridge network. They receive a
private Docker IP; `--publish 0:8000` creates a host-port forwarding rule from
the EC2 host's selected port to that private address on port 8000. The
control-plane container reaches that host port through Docker's `host-gateway`
mapping, exposed to it as `host.docker.internal`. The EC2 security group remains
the outer firewall. Public traffic enters Caddy on ports 80 and 443; keep port
8080 restricted to control-plane administration.

The initial migration creates a `deployments` table with UUID identifiers,
database-managed timestamps, and a PostgreSQL `deployment_status` enum. The
runtime is currently restricted to `python`; nullable container, port, and
public-identifier fields are reserved for future worker and routing milestones.

## Development database

Create an isolated Docker network and a persistent PostgreSQL container:

```sh
docker network create launchpad
docker volume create launchpad-postgres-data
docker volume create launchpad-workspaces
docker run -d --name launchpad-postgres --restart unless-stopped \
  --network launchpad \
  -e POSTGRES_DB=launchpad \
  -e POSTGRES_USER=launchpad \
  -e POSTGRES_PASSWORD=replace-this-development-password \
  -v launchpad-postgres-data:/var/lib/postgresql/data \
  postgres:17
```

The database has no published host port. A control-plane container on the same
Docker network reaches it using the `launchpad-postgres` hostname.

## Local development

Go 1.24 or later is required.

```sh
docker run --rm --name launchpad-postgres -p 5432:5432 \
  -e POSTGRES_DB=launchpad \
  -e POSTGRES_USER=launchpad \
  -e POSTGRES_PASSWORD=replace-this-development-password \
  postgres:17
```

In another terminal, run the service and test it:

```sh
LAUNCHPAD_DATABASE_URL='postgres://launchpad:replace-this-development-password@localhost:5432/launchpad?sslmode=disable' LAUNCHPAD_WORKSPACE_ROOT=/tmp/launchpad-workspaces LAUNCHPAD_PUBLIC_BASE_DOMAIN=apps.example.com go run ./cmd/control-plane
curl -i http://localhost:8080/health
curl -i -X POST http://localhost:8080/deployments \
  -H 'Content-Type: application/json' \
  -d '{"name":"example-api","runtime":"python","files":{"app.py":"from fastapi import FastAPI\napp = FastAPI()","requirements.txt":"fastapi\nuvicorn"}}'
```

Expected response:

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"status":"ok"}
```

To set configuration explicitly:

```sh
LAUNCHPAD_HOST=127.0.0.1 LAUNCHPAD_PORT=8081 LAUNCHPAD_LOG_LEVEL=debug LAUNCHPAD_DATABASE_URL='postgres://launchpad:replace-this-development-password@localhost:5432/launchpad?sslmode=disable' LAUNCHPAD_WORKSPACE_ROOT=/tmp/launchpad-workspaces LAUNCHPAD_PUBLIC_BASE_DOMAIN=apps.example.com go run ./cmd/control-plane
curl -i http://localhost:8081/health
```

## Docker

Build and run the control plane:

```sh
docker build -t launchpad-control-plane .
docker run --rm --name launchpad-control-plane --network launchpad -p 8080:8080 \
  -v launchpad-workspaces:/workspaces \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --add-host=host.docker.internal:host-gateway \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -e LAUNCHPAD_DATABASE_URL='postgres://launchpad:replace-this-development-password@launchpad-postgres:5432/launchpad?sslmode=disable' \
  -e LAUNCHPAD_WORKSPACE_ROOT=/workspaces \
  -e LAUNCHPAD_ROUTER_UPSTREAM_HOST=host.docker.internal \
  -e LAUNCHPAD_PUBLIC_BASE_DOMAIN=apps.example.com \
  launchpad-control-plane
```

Then use the same `curl` command above. Pass environment configuration with
`-e`, for example `-e LAUNCHPAD_LOG_LEVEL=debug`. The `stat -c` form shown is
for Amazon Linux on EC2; it adds the host Docker socket's group to the
non-root control-plane process. The `host-gateway` mapping is required on
Linux because `localhost` inside the control-plane container is not the EC2
host.
