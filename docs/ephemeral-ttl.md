# Milestone 16: ephemeral prototype TTL

New API deployments expire after seven days by default. Include `ttl_seconds` to
choose a different lifetime. Values from 1 second to 31,536,000 seconds (one year)
are accepted. Set it to `0` to retain a deployment indefinitely. If omitted or
`null`, Launchpad uses `LAUNCHPAD_DEFAULT_TTL_SECONDS` (default `604800`). TTL is
absolute from metadata creation and does not extend when the app receives traffic.
The API response includes `ttl_seconds` and `expires_at` (`null` for no expiry).

Example:

```json
{
  "name": "hello-world",
  "runtime": "python",
  "ttl_seconds": 604800,
  "files": {
    "app.py": "from fastapi import FastAPI\napp = FastAPI()",
    "requirements.txt": "fastapi\nuvicorn"
  }
}
```

## Expiration and retries

The control plane checks every 15 seconds by default (`LAUNCHPAD_TTL_CHECK_INTERVAL`).
Once due, it atomically changes the deployment to `EXPIRING`. Router lookups and
certificate authorization reject the URL as soon as `expires_at` passes, even if
the periodic scan has not yet run. Route-cache entries cannot outlive the deadline.
`EXPIRING` deployments cannot be started, woken, or manually deleted while cleanup
is underway.

The assigned worker serializes cleanup against builds, crash restarts and
scale-to-zero. It gracefully stops the deterministic container, removes it, removes
its deterministic image, and deletes the deployment workspace. Each operation is
safe to repeat when a response is lost. Only after worker cleanup succeeds does
PostgreSQL clear retained source and logs, release reserved capacity, and set
`EXPIRED`. A failed cleanup stores `expiration_error`, remains `EXPIRING`, and
retries with backoff. Inspect GET /deployments/ID or the structured control-plane
logs to see cleanup progress. The metadata row and public identifier remain for
audit; the expired public URL stays unavailable.

When Milestone 14 has confirmed a worker is FENCED, its containers cannot be running,
so Launchpad can finish expiration without reaching that machine. Its disk image,
workspace, and container records may remain on the stopped worker's EBS volume;
they cannot be removed through the private worker API. Retire that volume when it
is safe to do so. An old deployment with a container but no registered worker
assignment stays `EXPIRING` with an error rather than being reported expired while
leaving a potentially running container behind.

## Configuration and rollout

Control-plane settings:

```text
LAUNCHPAD_DEFAULT_TTL_SECONDS=604800
LAUNCHPAD_TTL_CHECK_INTERVAL=15s
```

Rebuild and update the control plane and both workers from the updated repository.
No AWS networking, DNS, security-group, or Caddy changes are needed. The worker
cleanup endpoint uses the existing authenticated private worker API and Docker
socket. Migration `010_deployment_ttl.sql` runs on control-plane startup. Back up
PostgreSQL before deploying the new migration.

```sh
# Control-plane EC2
docker build --target control-plane -t launchpad-control-plane:milestone-16 .

# Each worker EC2
docker build --target worker -t launchpad-worker:milestone-16 .
```

Before rollout, confirm all live deployments have registered workers and worker
assignments. If a cleanup RPC fails, keep the control plane running; it will retry.
You can retry DELETE for a deployment left in STOPPING. Do not manually change an
`EXPIRING` row to another state while the worker cleanup call may still be running.

## Acceptance checks

Create a disposable app with a short TTL:

```sh
curl -sS -X POST http://localhost:8080/deployments \
  -H 'Content-Type: application/json' \
  -d '{"name":"ttl-check","runtime":"python","ttl_seconds":60,"files":{"app.py":"from fastapi import FastAPI\napp=FastAPI()\n@app.get(\"/\")\ndef home(): return {\"ok\":True}\n","requirements.txt":"fastapi\nuvicorn\n"}}'
```

Save the ID, worker, container ID and public URL. Confirm the response gives an
`expires_at` about one minute in the future. After the deadline:

1. The public URL should return `410 Gone` once the deadline passes.
2. GET /deployments/ID should show `EXPIRING` while cleanup is underway and then
   `EXPIRED`; `public_url`, `container_id`, and `image_name` should be absent.
3. On the worker, confirm the container, image tag
   `launchpad/deployment:<deployment-id>-v<version>`, and workspace directory are
   gone. PostgreSQL should no longer retain `source_files` or runtime/build logs.
4. Restart the control plane during an `EXPIRING` retry and confirm it resumes.
   A worker cleanup failure should preserve the error and retry without recreating
   a container or image.
5. Create another app with `"ttl_seconds":0`; verify it has no `expires_at` and is
   not selected for expiration. Omit `ttl_seconds` on a third app and confirm the
   configured default applies.

TTL uses server/database time. The scan interval means cleanup and the status change
occur shortly after the exact expiry time; the URL check enforces the deadline
independently. Expiration is a hard deadline and can stop an in-flight request.
