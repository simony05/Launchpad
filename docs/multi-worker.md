# Milestone 11: multi-worker Launchpad

The control plane now schedules new deployments from the PostgreSQL worker
registry. It stores `worker_id` and a snapshot of `worker_address` on each
deployment before sending source code. Builds, stops, and reverse-proxy traffic
use that worker, not a global host. Caddy and public URLs stay on the control plane.
Migration `006_deployment_workers.sql` adds assignments and durable reservations.

## Scheduling algorithm

1. Parse the configured application CPU and memory limits (MiB/GiB).
2. Exclude UNHEALTHY workers and workers whose heartbeat exceeds
   `LAUNCHPAD_HEARTBEAT_TIMEOUT`, even if the monitor has not run yet.
3. Subtract outstanding deployment reservations from each worker's reported
   available CPU and memory. Require both remaining values to cover the request.
4. Choose the candidate with most remaining memory; break ties by worker UUID.
5. Atomically persist the assignment and reservation before contacting the worker.

A short PostgreSQL advisory lock serializes placement transactions across control
plane processes. Docker builds occur after committing, outside the lock. Heartbeat
updates cannot erase reservations. No worker means HTTP 503 with `deployment_id`
and an error; the record is FAILED and no Docker request is sent. There is no queue
or automatic retry; submit a new deployment after capacity becomes available.

Reservations are released on confirmed build failure, client setup failure, or
confirmed container removal. Successful running applications retain reservations.
Transport/start errors can mean a container exists despite an unsuccessful response,
so those reservations remain held. They are not automatically expired, recovered,
or moved to another worker. A cleanup failure is logged and also retains capacity.

### Tradeoffs

This favors workers with spare memory and is easy to inspect, but it is not
round-robin: unequal workers can receive unequal deployment counts. CPU only acts
as an eligibility filter. Current application limits apply to all new deployments;
there is no user-controlled per-deployment resource request yet.

Reservations are deliberately conservative. Running apps are already reflected in
heartbeats, so subtracting their reservations can count them twice. This prevents
stale-heartbeat oversubscription by this scheduler at the cost of underutilization.
It may reject a job that physically fits. Precise reconciliation would require
heartbeat allocation identities and is outside this milestone. Docker build
processes and host OS usage still are not bounded by these app reservations; keep
headroom for them. Independently launched Docker containers can consume resources
between reports. This is not a production resource guarantee.

If a failed request leaves a reservation without a recorded container ID, inspect
the assigned worker's deterministic `launchpad-<deployment-id>-v<version>` container
and any still-running build before manually releasing it. Never clear reservations
blindly. A stopped worker is not rescheduled; existing apps keep their assignments.

## EC2 layout and security groups

Use three Linux instances in the same VPC: the existing control plane, Worker A,
and Worker B. Keep private IPs stable for the life of existing assignments. Create
Worker B yourself in the AWS console; these instructions do not provision AWS.
Use a worker size with enough headroom for Docker builds, the worker service, and
apps. Both workers need Docker, Git, outbound access to image/package registries,
and the same repository revision. Ensure your SSH user has Docker group access.

Attach the same worker security group to A and B:

| Destination group | Inbound TCP | Source |
| --- | --- | --- |
| Worker | 8090 | Control-plane security group |
| Worker | Application published-port range | Control-plane security group |
| Worker | 22, if SSH is used | Your IP only |
| Control plane | 8091 | Worker security group |
| Control plane | 80, 443 | Public internet |
| Control plane | 8080, 22 | Your IP only |

Use `cat /proc/sys/net/ipv4/ip_local_port_range` on each worker to check its
ephemeral range (often 32768-60999); verify assigned Docker ports are covered by
the worker group's application-port rule. Do not open worker API or application
ports to the internet. Restrictive outbound rules must permit the corresponding
private traffic in the other direction. Both worker groups and control-plane
groups must be attached to the intended instances.

AWS security-group references authorize traffic using the instances' private IPs:
https://docs.aws.amazon.com/vpc/latest/userguide/security-group-rules.html

## Upgrade workers A and B

On EACH worker, in your checked-out repository, build the updated image:

```sh
git pull
docker build --target worker -t launchpad-worker:milestone-11 .
docker volume create launchpad-worker-workspaces
```

For A, reuse its Milestone 10 UUID. For B generate one once with:

```sh
cat /proc/sys/kernel/random/uuid
```

Save each UUID in that worker's deployment configuration. Never copy A's UUID to B.
Set these shell variables on each worker using its OWN values:

```sh
WORKER_ID='YOUR_STABLE_UUID'
WORKER_NAME='worker-a'
WORKER_PRIVATE_IP='THIS_WORKER_PRIVATE_IPV4'
CONTROL_PLANE_PRIVATE_IP='CONTROL_PLANE_PRIVATE_IPV4'
read -rs WORKER_TOKEN
```

At the prompt, paste the existing shared worker token and press Enter. For B use
`WORKER_NAME='worker-b'` and B's ID/private IP. Keep each workspace volume.
After building successfully, replace only the worker service container (this does
not remove generated application containers):

```sh
docker rm -f launchpad-worker
docker run -d --name launchpad-worker --restart unless-stopped \
  -p "$WORKER_PRIVATE_IP:8090:8090" \
  --cpus=0.25 --memory=128m \
  -v launchpad-worker-workspaces:/workspaces \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -e LAUNCHPAD_WORKSPACE_ROOT=/workspaces \
  -e LAUNCHPAD_WORKER_TOKEN="$WORKER_TOKEN" \
  -e LAUNCHPAD_WORKER_ID="$WORKER_ID" \
  -e LAUNCHPAD_WORKER_HOSTNAME="$WORKER_NAME" \
  -e LAUNCHPAD_WORKER_ADVERTISE_URL="http://$WORKER_PRIVATE_IP:8090" \
  -e LAUNCHPAD_CONTROL_PLANE_URL="http://$CONTROL_PLANE_PRIVATE_IP:8091" \
  -e LAUNCHPAD_HEARTBEAT_INTERVAL=10s \
  launchpad-worker:milestone-11
docker logs --tail 30 launchpad-worker
```

For a new B, `docker rm` will report no such container; proceed to `docker run`.
The worker service itself needs limits or it will report zero available capacity
under the conservative accounting introduced in Milestone 10.

## Upgrade the control plane

Build from the updated repository. Keep PostgreSQL's existing credentials, database,
volume and network. The migration runs on control-plane startup.

```sh
git pull
docker build -t launchpad-control-plane:milestone-11 .
CONTROL_PLANE_PRIVATE_IP='CONTROL_PLANE_PRIVATE_IPV4'
LEGACY_WORKER_PRIVATE_IP='WORKER_A_PRIVATE_IPV4'
read -rs WORKER_TOKEN
read -rs DATABASE_URL
```

Enter the shared worker token, then your existing PostgreSQL connection URL at the
two prompts. Replace the service after the build succeeds:

```sh
docker rm -f launchpad-control-plane
docker run -d --name launchpad-control-plane --restart unless-stopped \
  --network launchpad \
  -p 8080:8080 -p "$CONTROL_PLANE_PRIVATE_IP:8091:8091" \
  -e LAUNCHPAD_DATABASE_URL="$DATABASE_URL" \
  -e LAUNCHPAD_WORKER_TOKEN="$WORKER_TOKEN" \
  -e LAUNCHPAD_PUBLIC_BASE_DOMAIN=apps.runlaunchpad.dev \
  -e LAUNCHPAD_APP_CPUS=0.5 -e LAUNCHPAD_APP_MEMORY=256m \
  -e LAUNCHPAD_HEARTBEAT_TIMEOUT=45s \
  -e LAUNCHPAD_WORKER_URL="http://$LEGACY_WORKER_PRIVATE_IP:8090" \
  -e LAUNCHPAD_ROUTER_UPSTREAM_HOST="$LEGACY_WORKER_PRIVATE_IP" \
  launchpad-control-plane:milestone-11
docker logs --tail 30 launchpad-control-plane
```

The last two environment settings are optional legacy fallback ONLY for old
deployments without assignments. They must point to the worker hosting those apps.
New deployments always use the scheduler and cannot bypass capacity checks via
that fallback. Remove the settings once old deployments have been stopped or
recreated. New assignment snapshots are intentionally not changed by registration.

Caddy continues proxying to `launchpad-control-plane:8080`. If it still resolves
an old container IP after replacement, restart only `launchpad-caddy`; preserve
its certificate volume.

## Acceptance checks

1. Query the private registry from the control-plane host. Expect two distinct
   UUIDs, HEALTHY status, current heartbeats and sufficient available resources:

```sh
curl -sS -H "Authorization: Bearer $WORKER_TOKEN" \
  "http://$CONTROL_PLANE_PRIVATE_IP:8091/internal/workers"
```

2. Create several apps through the public control-plane API (restricted to your
   IP), or locally from the control-plane host:

```sh
curl -sS -X POST http://localhost:8080/deployments \
  -H 'Content-Type: application/json' \
  -d '{"name":"multi-worker-test","runtime":"python","files":{"app.py":"from fastapi import FastAPI\napp=FastAPI()\n@app.get(\"/\")\ndef root(): return {\"ok\":True}\n","requirements.txt":"fastapi\nuvicorn\n"}}'
```

3. Verify responses have a `worker_id`, `worker_address`, and working `public_url`.
   Check `docker ps` on the selected worker. The first two deployments need not
   land on different workers when capacities differ. Compare capacity and continue
   only while adequate headroom remains; resource-aware scheduling is not alternation.
4. Delete an app by ID. Verify its container disappears only on its assigned worker.
5. Stop B's worker service, wait beyond the heartbeat threshold, and create an app.
   It must choose A if A has capacity, or return 503. Existing B apps are not moved.
6. With no healthy eligible workers, POST returns 503 with a deployment ID and no
   Docker build starts. Restart worker services to restore registration.

## Automated verification

```sh
go test -race ./...
go vet ./...
LAUNCHPAD_TEST_DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/TEST_DB?sslmode=disable' \
  go test ./internal/scheduler -run TestPostgresPlacement -v
```

Use a disposable PostgreSQL database. The scheduling integration test creates an
isolated schema and verifies CPU/memory eligibility, most-memory selection,
stale/unhealthy exclusion, persisted assignment, concurrent reservations, heartbeat
updates, and reservation release. It skips when the database URL is not set.
