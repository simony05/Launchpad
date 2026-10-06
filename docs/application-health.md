# Milestone 13: application health and crash recovery

Workers periodically fetch their assigned RUNNING deployments from the private
control-plane registry, inspect each expected container, and report observations.
No worker database credentials, new public endpoints, or new security-group ports
are required. Migration `007_application_health.sql` runs on control-plane startup.

## Health states and recovery

`health_state` is separate from deployment lifecycle `status`:

- RUNNING: Docker reports the expected container running, and the optional HTTP
  check returns 2xx (or no HTTP check was requested).
- UNHEALTHY: HTTP timeout/non-2xx, or a missing/replaced managed container.
- EXITED: Docker reports that the expected container has exited.

HTTP checks are optional: POST /deployments may include `"health_path":"/health"`.
The path must be relative to the application's own origin, starting with `/`.
Probes go directly to the worker private IP and live host port, have a 2-second
timeout, do not follow redirects, and never include the shared worker token.
There is no application HTTP-check startup grace period: a slow startup can report
UNHEALTHY until a later successful probe. Probes do not restart live containers.

For an exited container, the worker records its exit code, OOM flag, failure message,
and a bounded log excerpt, then requests permission to restart it. The control
plane locks the deployment record and increments its durable restart count BEFORE
authorizing Docker start. Only matching worker/container/version and RUNNING
deployment records can claim attempts. Repeated/old claims cannot reset the budget.

Default policy: at most THREE attempts across the deployment's lifetime, with at
least 30 seconds between attempts. The budget does not reset after a healthy period
or worker/control-plane restart. A lost claim response conservatively consumes an
attempt without replaying it. The next observed exit after exhausting the budget
sets lifecycle status FAILED. A successful last attempt can continue running.
Setting the maximum to zero disables recovery and fails on the first observed exit.

The worker starts the same exited container; it does not rebuild, recreate, move,
or reschedule it. Docker restart policies must remain disabled (the default for
Launchpad-created apps). A missing/replaced container is UNHEALTHY and requires
manual attention, not an attempt to run a different container. A Docker API outage
is logged as a monitoring failure rather than a confirmed application crash.

Deleting through Launchpad removes the container and marks it STOPPED. It is then
excluded from monitoring, and late observations cannot resurrect it. A manual
`docker stop` or `docker kill` of a RUNNING deployment IS treated as an unexpected
exit and can cause recovery. Use DELETE /deployments/{id} for an intentional stop.

## Deployment API diagnostics

GET /deployments/{id} and GET /deployments include:

| Field | Meaning |
| --- | --- |
| `health_path` | Optional HTTP path; empty disables HTTP probes |
| `health_state` | Last RUNNING / UNHEALTHY / EXITED observation, initially null |
| `health_checked_at` | Database receipt timestamp; use it to detect stale reports |
| `restart_attempts` | Durable lifetime attempts claimed |
| `next_restart_at` | Earliest permitted next attempt; retained for diagnostics |
| `last_exit_code`, `oom_killed` | Most recently reported exit details |
| `runtime_error` | Current unhealthy/crash reason; cleared on a healthy report |
| `last_failure` | Last failure message, retained after recovery |
| `runtime_logs` | At most 4 KiB from the last 50 log lines at exit |

The log excerpt may be truncated and may contain sensitive text printed by the
application; treat it as untrusted application output. No runtime log streaming or
unbounded event history is added. Crashes before initial startup completes still
use the existing `start_error` path and are not enrolled in recovery.

Unhealthy/exited observations make cached application routes unavailable after
their normal short TTL; subsequent healthy reports restore routing. FAILED apps
remain unavailable. Reservations are retained until explicit deletion/confirmed
cleanup, preventing uncertain recoveries from overcommitting workers.

## Configuration

| Service | Variable | Default |
| --- | --- | --- |
| Worker | `LAUNCHPAD_APP_HEALTH_INTERVAL` | `10s` |
| Control plane | `LAUNCHPAD_MAX_RESTART_ATTEMPTS` | `3` (range 0-10) |
| Control plane | `LAUNCHPAD_RESTART_COOLDOWN` | `30s` |

Intervals/cooldown range from 1s to 1h. Workers scan serially; many apps or slow
probes can make a scan exceed its interval. Registry requests have a 5-second
timeout. If the control plane/database is unavailable, workers do not restart apps
without authorization; they log failures and retry the next scan. This fails closed
and preserves the restart cap. One worker process per stable worker UUID is expected.

## Upgrade

Build both images from the new revision:

```sh
# Control-plane EC2
git pull
docker build -t launchpad-control-plane:milestone-13 .
# Each worker EC2
git pull
docker build --target worker -t launchpad-worker:milestone-13 .
```

Recreate the control-plane container first using its Step 12 command with the new
tag; this applies migration 007 and enables the authenticated registry endpoints:

```text
GET  /internal/workers/{id}/applications
POST /internal/workers/{id}/applications
```

Then recreate both worker services using their existing Step 12 run commands and
the new tag. Preserve worker UUIDs, shared token, workspace volumes and private
bind IPs. Defaults enable monitoring automatically; no extra flags are required.
Existing assigned RUNNING deployments are enrolled, including those without an
HTTP path. Pre-assignment legacy deployments are not automatically adopted.

## Acceptance checks

Create a test app with a health endpoint and a controlled crash endpoint from the
control-plane EC2 (the crash endpoint is strictly a temporary test fixture):

```sh
curl -sS -X POST http://localhost:8080/deployments \
  -H 'Content-Type: application/json' \
  -d '{"name":"recovery-test","runtime":"python","health_path":"/health","files":{"app.py":"import os, threading\nfrom fastapi import FastAPI\napp=FastAPI()\n@app.get(\"/health\")\ndef health(): return {\"ok\": True}\n@app.post(\"/crash\")\ndef crash():\n    threading.Timer(0.5, lambda: os._exit(42)).start()\n    return {\"crashing\": True}\n","requirements.txt":"fastapi\nuvicorn\n"}}'
```

1. Save its deployment ID and public URL. After about 10 seconds GET /deployments/ID
   should show health_state RUNNING and zero restart attempts.
2. POST to PUBLIC_URL/crash once. Observe EXITED and restart_attempts 1, then RUNNING
   again. last_exit_code should be 42 and last_failure should remain populated.
3. Repeat after recovery until three attempts have been used. Crash a fourth time:
   deployment status becomes FAILED; no further starts occur. Keep querying for a
   minute and confirm restart_attempts stays at 3.
4. Restart the worker service and verify that it does not reset the count or recover
   the FAILED deployment. Delete that test deployment to remove its container.
5. Deploy an app whose /health returns HTTP 500. It should become UNHEALTHY, return
   503 through routing after cache expiry, and retain restart_attempts 0. Change
   the application's behavior to return 200 and observe health/routing recovery.
6. Delete a healthy test deployment via the API. It must stay STOPPED after several
   scans and a worker restart. No container should reappear.

## Automated verification

```sh
go test -race ./...
go vet ./...
# Use a disposable PostgreSQL database for persistent budget/cooldown tests:
LAUNCHPAD_TEST_DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/TEST_DB?sslmode=disable' \
  go test ./internal/workers -run TestPersistentRecoveryBudget -v
```

The database test verifies claimed-attempt persistence, duplicate rejection,
cooldown, final FAILED state and stopped-deployment protection. It skips when no
test database is configured. Live Docker recovery and network behavior still need
the EC2 acceptance checks above.
