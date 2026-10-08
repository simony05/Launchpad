# Milestone 15: scale idle prototypes to zero

Scale-to-zero stops application containers, not worker EC2 instances. A stopped
container consumes no application-process CPU/RAM, but its image and writable
filesystem remain on the worker disk. The deployment, source, image name, resource
requirements, worker assignment, container ID, version and public URL are preserved.
Process memory and in-process sessions do not survive a stop/start.

## Lifecycle

```text
RUNNING -> SUSPENDING -> SLEEPING -> WAKING -> RUNNING
```

The intermediate states record intent before a worker operation. A controller
restart resumes pending transitions. The worker checks deployment ID, version,
idle-operation epoch and assignment before changing Docker state. It uses
`docker stop --time 10` and `docker start`, not remove/rebuild/run. Retries inspect
the existing container and do not start a second copy. The newly inspected port
is saved because Docker's dynamically allocated host port may change.

`last_request_at` is persisted at request admission and completion. Completion
also extends the idle window, so a long response is not stopped immediately after
finishing. The router tracks active requests until reverse proxying completes;
streams and WebSockets therefore prevent idle shutdown. Health probes, deployment
API calls and Caddy certificate authorization do not count as application traffic.
Probes do not wake sleepers. Background jobs alone do not count as traffic and
will be interrupted by idle shutdown; this feature targets request-driven apps.

On incoming traffic, the router waits for a single serialized wake operation
without consuming the original request body or replaying the original request.
Concurrent arrivals wait for the same transition. Cancelling one caller does not
cancel an already-started wake. Other waiting callers can cancel independently.
After Docker reports running, Launchpad polls readiness every 200ms:

- With `health_path`, require HTTP 2xx. Probes have a 2s timeout, send no worker
  credential, and do not follow redirects.
- Without `health_path`, require a successful TCP connection to the app port.
  Set `health_path` for stronger application-level readiness.

Successful wakes persist `cold_start_ms` and emit a structured `application cold
start` log with deployment ID, version and `latency_ms` (worker RPC plus readiness
time for that attempt). The API also exposes `idle_error`. Readiness failures on
a healthy worker mark FAILED; repeated uncertain worker operations are bounded
to three attempts, then FAILED with diagnostics. The container/reservation is
retained conservatively for inspection and normal DELETE cleanup. Shutdown leaves
in-progress work resumable rather than treating cancellation as an app failure.

## Capacity, races and routing

Capacity is released only after confirming the container is stopped. Waking reserves
CPU/memory on the original worker under the scheduler's placement lock. No capacity
means HTTP 503 with Retry-After; the app remains SLEEPING until a later request can
reserve it. Resource heartbeats are conservative, so a freshly sleeping app may
need to wait for the next worker report before capacity is advertised as free.
This milestone does not move sleeping containers to another healthy worker just
to find spare capacity, since doing so would discard their writable filesystem.

The router still caches verified locations. Warm requests add PostgreSQL activity
writes at admission and completion, without another full metadata/location read.
Sleeping requests also read lifecycle metadata. This is an intentional
prototype-scale tradeoff; traffic batching and distributed request leases are not
implemented. Bounded lock stripes coalesce wake-ups without growing a lock map;
unrelated apps sharing a stripe may wait behind a cold start or delay sleeping.

Run **one control-plane/router process**, as in the current architecture. An
exclusive PostgreSQL advisory lock rejects a second scale-to-zero owner; loss of
that connection shuts down the owner. Do not run additional routers with the
feature disabled against the same deployments, because their traffic would not
be counted. A future multi-router design needs shared request leases.

DELETE claims STOPPING before contacting the worker. WAKING/SUSPENDING/RECOVERING
are busy states and return 409. This prevents a delayed sleep/wake from reviving a
deleted deployment. A failed delete can be retried while STOPPING. Worker crash
monitoring shares locks with sleep/wake and only restarts deployments still RUNNING,
so an intentional stop is not treated as a crash.

If a worker fails while apps sleep, they remain SLEEPING. Traffic marks the affected
app WAKING once the worker is FENCED; milestone 14 can then rebuild it elsewhere.
This exceptional path may return 503 until recovery finishes and requires durable
source, recovery enabled, and confirmed fencing. It cannot preserve files on the
failed worker. Running or waking apps still participate in worker-failure recovery.

## Upgrade

1. Back up PostgreSQL. Deploy the updated control plane with scale-to-zero disabled
   (the default). Migration `009_idle.sql` runs automatically and exposes the new
   private operation-authorization endpoint.
2. Upgrade **both workers**, preserving their UUIDs, instance IDs, token, workspace
   volumes, limits and private bind IPs. No additional worker environment variables
   are needed. The existing Docker socket mount permits stop/start.
3. Recreate the single control-plane container with the following additional
   environment settings, retaining all previous database, worker, router and
   failover settings:

```text
LAUNCHPAD_SCALE_TO_ZERO_ENABLED=true
LAUNCHPAD_IDLE_TIMEOUT=15m
LAUNCHPAD_IDLE_CHECK_INTERVAL=30s
LAUNCHPAD_COLD_START_TIMEOUT=60s
```

Build the appropriate target on each instance from the updated repository:

```sh
# Control-plane EC2
docker build --target control-plane -t launchpad-control-plane:milestone-15 .

# Each worker EC2
docker build --target worker -t launchpad-worker:milestone-15 .
```

Idle timeout and scan interval accept durations from 1s to 1h; cold-start timeout
accepts 1s to 2m. The default idle window is 15 minutes. For a short acceptance
test use idle timeout `60s` and check interval `5s`. Actual sleep occurs after the
idle window plus scan/operation delay, not at an exact deadline.

Public URLs and TLS authorization continue working for sleeping applications.
No DNS, security-group, or Caddy routing changes are required. Any custom client
or edge response timeout must exceed the cold-start timeout. The control-plane
HTTP read/write timeouts are extended when enabled so a POST body can wait through
a cold start. Do not disable the feature while applications remain SLEEPING;
wake/delete them first or leave the feature enabled with a longer idle timeout.

## Acceptance checks

Use a disposable test application with a `/health` endpoint and set
`"health_path":"/health"` in POST /deployments. Save its deployment ID and public
URL, then perform these checks yourself on EC2:

1. Request the public URL; verify `last_request_at` changes. Stop browser polling
   and wait beyond the idle timeout plus one scan.
2. GET /deployments/ID must show SLEEPING, the same container ID/public URL, and a
   null host_port. `docker ps -a` on its worker must show that container stopped.
3. Request the same URL again. It should wait briefly and then return the app's
   response. Inspect RUNNING, `cold_start_ms`, and the new host port. The same
   container ID and version should remain; no image rebuild is required.
4. Let it sleep again and issue ten simultaneous requests. All should receive the
   app response, with one wake transition and one retained app container.
5. Keep a streaming request open beyond the idle timeout. The app must remain
   RUNNING. After closing it, wait a full idle window before expecting sleep.
6. Delete a sleeping deployment. Requests must not revive it. Test an app whose
   health endpoint never becomes ready and verify bounded waiting plus idle_error.

From a shell with your values set:

```sh
curl -sS "http://localhost:8080/deployments/$DEPLOYMENT_ID"
curl -sS --max-time 90 -w '\nTotal request seconds: %{time_total}\n' "$PUBLIC_URL/health"
printf '%s\n' 1 2 3 4 5 6 7 8 9 10 | \
  xargs -P 10 -I '{}' curl -fsS --max-time 90 "$PUBLIC_URL/health"
docker logs --since 10m launchpad-control-plane
```

Automated tests use a disposable PostgreSQL database and fake worker RPCs; they
exercise real migrations, reservations, concurrent wake-ups, active-request
protection, interrupted transitions, deletion, and original request forwarding:

```sh
LAUNCHPAD_TEST_DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/TEST_DB?sslmode=disable' \
  go test -race ./...
go vet ./...
```

These do not replace the live EC2/Docker acceptance checks. No production-grade
hostile multi-tenant isolation or exactly-once application side effects are claimed.
