# Worker registration and heartbeats

The worker registers on startup and sends a report every 10 seconds. Registration
and heartbeat requests are idempotent PostgreSQL upserts keyed by a stable UUID.
Keep the same UUID across worker container restarts. Do not share an ID between
machines. No database credentials are sent to the worker.

Migration `005_create_workers.sql` adds identity, hostname, advertised API address,
HEALTHY/UNHEALTHY status, total/available CPU and memory, running container count,
and a database-generated `last_heartbeat`. Memory is in bytes; CPU is in cores
(fractional values are supported). A separate monitor marks stale rows unhealthy.
Receiving a healthy report restores HEALTHY automatically. Docker collection
failure sends an unhealthy report with zero capacity rather than stale capacity.

## Capacity semantics

Availability is total Docker-host capacity minus the configured limits of all
running containers, clamped at zero. CPU quota/period is supported as well as
NanoCPUs. An unlimited container consumes all advertised availability for that
resource. This includes the worker container: give it CPU and memory limits.
These are conservative allocation estimates, not instantaneous utilization or
OS free memory. Builds and host processes are not accounted for; these values
must not be treated as a guarantee that another application will fit.

## Configuration

Control plane:

| Variable | Default | Meaning |
| --- | --- | --- |
| `LAUNCHPAD_REGISTRY_ADDRESS` | `0.0.0.0:8091` | Separate internal HTTP listener |
| `LAUNCHPAD_HEARTBEAT_TIMEOUT` | `45s` | Maximum age of a healthy heartbeat |
| `LAUNCHPAD_HEARTBEAT_CHECK_INTERVAL` | `5s` | Stale-worker scan frequency |

Worker (in addition to existing workspace and token settings):

| Variable | Default | Meaning |
| --- | --- | --- |
| `LAUNCHPAD_WORKER_ID` | required | Stable, nonzero UUID |
| `LAUNCHPAD_WORKER_HOSTNAME` | OS hostname | Prefer the EC2 hostname over a container ID |
| `LAUNCHPAD_WORKER_ADVERTISE_URL` | required | Worker private API URL, e.g. `http://172.31.1.20:8090` |
| `LAUNCHPAD_CONTROL_PLANE_URL` | required | Registry private URL, e.g. `http://172.31.1.10:8091` |
| `LAUNCHPAD_HEARTBEAT_INTERVAL` | `10s` | Report frequency |

Durations accept `s`/`m`, range 1 second to 1 hour. Set the failure timeout well
above the report interval (e.g. 45s versus 10s). A missed heartbeat is detected
within approximately timeout + check interval, assuming the database is available.
Each report uses bounded Docker collection and HTTP timeouts, and retries on the
next interval. Collection and HTTP timeouts can delay a report beyond its interval.
If PostgreSQL is unavailable the monitor logs errors and retries; it cannot update
stored status until the database recovers. Both loops stop on process shutdown.

## EC2 network changes

On the CONTROL PLANE security group add **Custom TCP 8091**, source the WORKER
security group. Use instances in the same VPC and private IPv4 addresses. If
outbound rules are restricted, allow worker egress to the control-plane group on
8091 too. Do not expose 8091 publicly or proxy it through Caddy. Existing worker
8090 and application-port rules from the control-plane security group stay in place.

Every registry endpoint requires the existing shared `LAUNCHPAD_WORKER_TOKEN`:

- `POST /internal/workers/register`
- `POST /internal/workers/heartbeat`
- `GET /internal/workers`

The registry is not mounted on public port 8080. This is private-VPC HTTP with
bearer authentication, not encrypted transport or per-worker credentials. The
shared token trusts its holder to report worker identity and capacity.

## Upgrade

Update the code on both instances and build from the repository directory:

```sh
# Control plane EC2
docker build -t launchpad-control-plane:milestone-10 .
# Worker EC2
docker build --target worker -t launchpad-worker:milestone-10 .
```

Recreate the control-plane container with its existing database URL, shared token,
worker URL, router host, public base domain, Docker network and 8080 mapping.
Add `-p CONTROL_PLANE_PRIVATE_IPV4:8091:8091` and the optional timeout variables
above; use the new image tag. Do not add the Docker socket or a workspace mount.
Startup applies the migration. Caddy requires no change.

Generate a UUID once on the worker using `cat /proc/sys/kernel/random/uuid`.
Save it in your deployment configuration and use it when recreating the container.
On the worker, retain the existing workspace volume and Docker socket mount;
add these flags to the existing `docker run` command, using your actual values:

```sh
--cpus=0.25 --memory=128m \
-e LAUNCHPAD_WORKER_ID='YOUR_STABLE_UUID' \
-e LAUNCHPAD_WORKER_HOSTNAME='worker-1' \
-e LAUNCHPAD_WORKER_ADVERTISE_URL='http://WORKER_PRIVATE_IPV4:8090' \
-e LAUNCHPAD_CONTROL_PLANE_URL='http://CONTROL_PLANE_PRIVATE_IPV4:8091' \
-e LAUNCHPAD_HEARTBEAT_INTERVAL=10s
```

The flags above are part of a `docker run` command, not a standalone command.
Use image `launchpad-worker:milestone-10`. Existing generated containers are not
restarted by registration. The configured single-worker routing and execution
destination remain unchanged; registry status does not gate deployment requests
or trigger rescheduling in this milestone.

## Verify on EC2

On the control-plane EC2, query the private listener (set the token locally):

```sh
read -rs LAUNCHPAD_WORKER_TOKEN
curl -i -H "Authorization: Bearer $LAUNCHPAD_WORKER_TOKEN" \
  http://CONTROL_PLANE_PRIVATE_IPV4:8091/internal/workers
```

Expect one HEALTHY row with a recent heartbeat. Stop only the worker service with
`docker stop launchpad-worker`, wait about 50 seconds, and query again: UNHEALTHY.
Run `docker start launchpad-worker` and query again: the same UUID returns to HEALTHY.
Generated application containers keep running throughout this test.

## Tests

```sh
go test ./...
go test -race ./internal/workers ./internal/worker ./internal/containers
# Optional: use a disposable PostgreSQL database; the test runs migrations.
LAUNCHPAD_TEST_DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/TEST_DB?sslmode=disable' go test ./internal/workers -run TestPostgresHeartbeatLifecycle -v
```

The database test verifies upsert, stale expiry, heartbeat recovery, and reported
unhealthiness. It is skipped when no test database URL is set.
