# Milestone 12: private routing across workers

Public requests enter Caddy, pass to the Go router, then reach the assigned
worker's private IP and the container's current published host port:

```text
https://<public_identifier>.apps.runlaunchpad.dev/path?query
    -> Caddy :443
    -> control-plane router :8080
    -> Worker B private IPv4 :32781
    -> container :8000
```

## Route verification

On a cache miss, the control plane reads deployment metadata and its worker's
registry record. The worker must be HEALTHY with a heartbeat newer than
`LAUNCHPAD_HEARTBEAT_TIMEOUT`. The stored assignment address must still match
registration: an address change is rejected rather than silently routing an old
container port to a different machine.

The router then calls the worker's authenticated status API (3-second HTTP timeout,
5-second overall lookup timeout). The deployment UUID, version and container ID
must match, the container must be running, and its published port must be valid.
The returned live port supersedes a stale metadata port in the routing cache.
This does not rewrite deployment records, restart containers, or reschedule jobs.
Legacy deployments without a worker ID use `LAUNCHPAD_WORKER_URL` and a live status
check, but cannot receive a registry health check until they have an assignment.

Only literal private IP worker addresses are accepted for application routing.
Private DNS names and public worker addresses are not accepted in this version.

## Cache and errors

- Successful routes: `LAUNCHPAD_ROUTER_CACHE_TTL_SECONDS`, default 5 seconds.
- Missing/unavailable routes: 1 second. Recovery is retried after this interval.
- Maximum 1,024 cached routes; expired entries are removed on capacity pressure.
- Concurrent cache misses are coalesced with a fixed set of locks. Unrelated
  identifiers can share a lock, so a slow lookup can delay another cache miss.
- API deletion invalidates the route; in-flight lookups cannot reinsert it after
  invalidation completes. Already-forwarded requests are not recalled.
- Proxy connection failures evict the route. The next request rechecks metadata
  and status. The router does not explicitly retry/replay the failed request.
- Caddy's TLS authorization checks reuse the route cache.

Unknown identifiers return 404. Stopped/missing/replaced containers, unhealthy
workers, expired heartbeats, unavailable status APIs and invalid locations return
503. An upstream transport failure after a successful lookup returns 502 and
invalidates the route. Application-generated HTTP errors are passed through and
do not invalidate it. New TLS certificate issuance is denied for unavailable routes;
clients without an existing certificate may see a TLS error instead of HTTP 503.

Dial timeout is 2 seconds, response-header timeout 10 seconds, and idle pooled
connections expire after 60 seconds. WebSocket upgrades/streaming use Go's reverse
proxy; the response-header timeout only applies before headers arrive.

Healthy cache entries can conceal worker/container state changes for up to the
configured TTL. The registry freshness decision can also be cached for that period.
Container status and port use are not atomic: a port could be reused after a check.
This bounded-staleness design is suitable for this educational runtime, not a
hostile multi-tenant identity/isolation guarantee. No PostgreSQL or status API call
is needed for a valid cached route.

## Required EC2 networking

On BOTH worker security groups, allow TCP 8090 and the assigned application port
range ONLY from the control-plane security group. Remove public CIDR allowances
for those ports, including IPv6 allowances, from every group attached to the worker.
Security-group permissions are additive. Keep SSH limited to your IP.
Check the host's ephemeral port range with:

```sh
cat /proc/sys/net/ipv4/ip_local_port_range
```

Keep control-plane ports 80/443 public for Caddy, 8080 restricted for administration,
and 8091 allowed from the worker security group. All worker communication uses
private VPC addresses. No public DNS, Caddy, or load-balancer changes are required.

Workers now publish apps with `--publish PRIVATE_IPV4:0:8000` rather than all host
addresses. IMPORTANT: EC2 public IPv4/Elastic IP traffic is translated to the private
interface. The binding alone does NOT prevent public access; the security group is
the required public-access barrier. Keep Docker's normal bridge networking and do
not enable unrestricted direct routing to container subnets.

References:
- https://docs.docker.com/engine/network/port-publishing/
- https://docs.aws.amazon.com/vpc/latest/userguide/security-group-rules.html

## Upgrade

No new SQL migration is needed. Preserve the Step 11 database, worker UUIDs,
shared token, workspace volumes and assignments. Build on each instance from
the updated repository:

```sh
# Each worker
git pull
docker build --target worker -t launchpad-worker:milestone-12 .
# Control-plane instance
git pull
docker build -t launchpad-control-plane:milestone-12 .
```

Recreate each worker service using its Step 11 run command, changing its image tag
to `milestone-12` and adding the following environment option BEFORE the image:

```sh
-e LAUNCHPAD_APP_BIND_IP="$WORKER_PRIVATE_IP"
```

The value must be that worker EC2's private IPv4 (not its Docker container IP),
matching the IP in `LAUNCHPAD_WORKER_ADVERTISE_URL`. Missing/public/wildcard bind
addresses cause worker startup to fail. New apps bind to that address. Existing
apps retain their old port bindings; the tightened security groups protect them.
Recreate old apps through the deployment API to change their Docker bindings; do
not merely restart the worker and expect existing app bindings to change.

Recreate the control-plane service using its Step 11 run command with image
`launchpad-control-plane:milestone-12`. Optionally add:

```sh
-e LAUNCHPAD_ROUTER_CACHE_TTL_SECONDS=5
```

Restart Caddy only if it retains the removed control-plane container's old IP.
Do not delete its certificate data. Build/update workers before the control plane
so the live status API supports stopped containers with no port mappings.

## Acceptance checks

1. Deploy apps on both workers using the Step 11 checks. Their public URLs must
   work and `worker_id` must match the instance holding the container.
2. On that worker, run `docker port CONTAINER_ID 8000/tcp`. A newly created app
   should show its private IP plus assigned host port.
3. From the control-plane EC2, `curl http://WORKER_PRIVATE_IP:HOST_PORT/` should
   work. From your Mac, `curl --connect-timeout 5 http://WORKER_PUBLIC_IP:HOST_PORT/`
   must fail while `curl https://PUBLIC_IDENTIFIER.apps.runlaunchpad.dev/` works.
4. On a test app only, run `docker stop CONTAINER_ID`. Its public URL should return
   502 initially if the cached route is used, then 503 after invalidation/expiry.
   Other apps keep working. Start that container again; after the cache expires,
   the router discovers its current port and serves it again.
5. Stop only the worker service. After the successful cache expires, its apps return
   503 because the status API is unavailable, even before heartbeat timeout. Restart
   the service; healthy registration and successful status checks restore routing.
6. Delete an app through the deployment API. Subsequent requests return 503 without
   waiting for the old successful cache entry to expire.
7. Request the same running app repeatedly: the worker logs should show status
   requests at cache refreshes, not on every public request. Registry/deployment
   lookups follow the same cache-miss boundary.

Use test apps for interruption checks. Application routing errors never migrate
workloads or release scheduler reservations.

## Local verification

```sh
go test -race ./...
go vet ./...
```

Tests cover host-based routing, current-port selection, changed identities/addresses,
worker unavailability/staleness, stopped-container inspection, cache hits/expiry,
negative caching, bounded cache size, concurrent misses, failure invalidation, and
private Docker binding. Live AWS security-group behavior requires the EC2 checks.
