# Milestone 14: worker failure recovery

## Safety model

A missing heartbeat proves loss of communication, not that containers stopped.
Starting duplicates immediately could let both copies write external data.
Launchpad therefore uses **confirmed EC2 stop as fencing**: the old machine must
be stopped before another worker starts any of its deployments. This intentionally
trades availability for safety when AWS or fencing permissions are unavailable.
It does not provide exactly-once application side effects or hostile-tenant isolation.

1. After the heartbeat timeout (default 45s), routing/scheduling rejects the worker.
2. After a separate recovery grace (180s since the last heartbeat), the control
   plane atomically quarantines it as FENCING. A fresh heartbeat before that claim
   cancels suspicion. After the claim, heartbeats cannot revive the worker.
3. Verify the registered EC2 ID, private IP and worker identity tags. Request a
   normal EC2 stop; poll until DescribeInstances reports `stopped`. A successful
   StopInstances response alone is not sufficient. No force-stop is used.
4. Mark the worker FENCED. For each active deployment, reserve a healthy worker
   using the existing CPU/memory algorithm, set RECOVERING, increment execution
   `version`, and persist the new assignment before making a worker request.
5. Rebuild from immutable source stored in PostgreSQL, start the container, and
   record its new private address/port. Set RUNNING and invalidate routing cache.
   The deployment ID, public identifier, health path and public URL stay the same.

Recovery scans run every 10s, one fencing attempt and one deployment per scan.
Builds serialize recovery work; large fleets are intentionally out of scope.
The control plane also waits a full grace period after startup, allowing workers
to re-register after a shared outage. The grace is not an exact recovery SLA:
add scan delay, EC2 shutdown time, capacity wait and rebuild time.
Choose it comfortably above ordinary network interruptions and worker-service
maintenance. Raising it reduces unnecessary instance stops but increases downtime.

## Races, retries and limitations

- PostgreSQL serializes recovery controllers with a session advisory lock. Placement
  uses the same transaction lock as normal scheduling, so reservations are atomic.
- Assignments are persisted before RPCs. A restarted controller resumes RECOVERING
  work. Workers verify assignment and version before building and before starting;
  unavailable registry means no new start. Deterministic container names and
  per-deployment locks make same-generation retries reuse a running container.
- After a lost response, the controller checks live status before replaying Start.
  Three RPC attempts per assignment are allowed; the lifetime worker failover
  budget defaults to three. Unknown outcomes retain capacity conservatively and
  expose `recovery_error`; operators must inspect the assigned worker.
- Old health reports cannot update new versions. Old deletion/reservation-release
  operations cannot stop or release the new generation. DELETE during RECOVERING
  returns a conflict; it is not a recovery-cancellation API.
- A failed replacement build records build output/error and becomes FAILED.
  No capacity leaves the deployment RECOVERING and retries placement on later scans.
  Failed/stopped applications are not resurrected by worker recovery.
- Cache invalidation is immediate in the recovering process. Other replicas use
  the existing bounded cache TTL. During downtime requests fail; connections and
  in-flight requests are not migrated or automatically retried.
- Local files, writable container layers, RAM and app-side state are **not** moved.
  Rebuilding unpinned dependencies can produce a different image. Persist important
  app data externally and pin dependencies; shared image storage is not added here.
- Pre-milestone-14 deployments lack durable source and fail with an actionable
  error after fencing. Redeploy them through POST /deployments before a recovery test.
- Workers without `instance_id` remain ineligible for automatic fencing/recovery.
  Workers in FENCING/FENCED are never automatically reactivated. An AWS API failure,
  tag/IP mismatch or prolonged `stopping` leaves recovery blocked with `fencing_error`.
- This assumes trusted operators, workers and IAM. Do not use external automation
  that restarts fenced instances. Manually starting an old instance can violate the
  fencing guarantee. Replace fenced nodes with fresh EC2 instances and new worker
  UUIDs; do not simply restart/re-register the old machine. Retire old disks safely.

## Upgrade and AWS configuration

Automatic worker recovery is **off by default**. Nothing in the local tests calls
AWS. Enabling it authorizes the running control plane to STOP registered workers.
The control-plane instance must never carry the worker role tag or be included
in its stop-permission resource list.

1. Back up PostgreSQL. Build/recreate the control plane first with recovery disabled;
   startup runs migration `008_worker_failover.sql` and adds the assignment endpoint.
2. Build/recreate both workers preserving UUIDs, tokens, private IPs, limits and
   workspace volumes. Add `LAUNCHPAD_WORKER_INSTANCE_ID` with that worker's EC2
   instance ID. Old control planes cannot authorize upgraded worker starts.
3. In the EC2 console, tag each worker with `launchpad:role=worker` and
   `launchpad:worker-id=<its existing LAUNCHPAD_WORKER_ID UUID>`.
4. Attach a narrowly scoped IAM role to the control-plane EC2 instance. Use the
   policy below with your region/account and the exact TWO WORKER instance ARNs.
   Do not give generated applications or worker instances this role.
5. Ensure the control-plane container can obtain instance-role temporary credentials
   and reach the regional EC2 HTTPS API. Use IMDSv2; container networking may need
   an instance metadata response hop limit of 2. Do not bake AWS keys into images.
6. Inspect the private worker list and confirm healthy reports with the expected
   instance IDs. Then recreate the control-plane container with the settings below.
   Keep all existing database, registry, router, token and resource settings.

No new public ports, DNS records, or Caddy changes are required. Keep registry8091,
worker8090 and app ports private under the existing security groups. EC2 API calls
are outbound HTTPS; generated app container ports remain private.

```sh
# From the updated repository on the corresponding EC2:
docker build --target control-plane -t launchpad-control-plane:milestone-14 .
docker build --target worker -t launchpad-worker:milestone-14 .
```

Additional worker `docker run` option (use its actual ID):

```sh
-e LAUNCHPAD_WORKER_INSTANCE_ID=i-0123456789abcdef0
```

Additional control-plane `docker run` options (continuation lines, not standalone commands):

```sh
  -e AWS_REGION=us-east-1 \
  -e LAUNCHPAD_WORKER_RECOVERY_ENABLED=true \
  -e LAUNCHPAD_WORKER_RECOVERY_GRACE=180s \
  -e LAUNCHPAD_WORKER_RECOVERY_INTERVAL=10s \
  -e LAUNCHPAD_WORKER_RECOVERY_MAX_ATTEMPTS=3 \
```

IAM policy template; replace all placeholder ARNs before applying yourself:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "ec2:DescribeInstances",
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "ec2:StopInstances",
      "Resource": [
        "arn:aws:ec2:REGION:ACCOUNT:instance/WORKER_A_INSTANCE_ID",
        "arn:aws:ec2:REGION:ACCOUNT:instance/WORKER_B_INSTANCE_ID"
      ],
      "Condition": {"StringEquals": {"ec2:ResourceTag/launchpad:role": "worker"}}
    }
  ]
}
```

The SDK uses the standard credential chain. Keep the control plane isolated from
untrusted workloads, and restrict worker access to IMDS and other private services.
Docker alone is not production-grade hostile multi-tenant isolation.

## Acceptance checks

Perform these only on disposable test workers/apps; recovery will stop the EC2.

1. Deploy a new test app and save its ID, version, worker ID and public URL. Confirm
   another worker has sufficient spare CPU/memory after reservations.
2. On the assigned worker, `docker stop launchpad-worker` (only the worker service,
   leaving apps running). Within 45s plus the heartbeat scan the worker becomes
   UNHEALTHY; before 180s it must not move. Restart the service before that deadline
   to check transient-loss recovery without reassignment.
3. Repeat and leave it stopped. After the grace, inspect FENCING and the EC2 state
   `stopping`. No replacement may start until AWS reports `stopped`.
4. Poll GET /deployments/ID: expect RECOVERING, new worker ID/version, then RUNNING.
   The original public URL must serve the replacement. Check `failover_attempts=1`.
5. With stop permission deliberately absent on a separate disposable test, verify
   FENCING plus an error, no replacement, and no duplicate container. Restore the
   permission to let the pending fencing operation finish.
6. Test insufficient target capacity: RECOVERING should wait without incrementing
   failover attempts repeatedly. Free capacity and observe eventual recovery.

Inspect workers from the control-plane EC2, using the existing worker secret:

```sh
curl -sS http://localhost:8091/internal/workers \
  -H "Authorization: Bearer $LAUNCHPAD_WORKER_TOKEN"
curl -sS http://localhost:8080/deployments/DEPLOYMENT_ID
```

Local verification (use a disposable PostgreSQL database):

```sh
LAUNCHPAD_TEST_DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/TEST_DB?sslmode=disable' \
  go test -race ./...
go vet ./...
```

Tests fake EC2 and worker RPCs but exercise real PostgreSQL migration, grace,
quarantine, capacity waiting, durable source, assignment generations and retry
reconciliation. They do not perform real EC2 stop/start operations.

References: [EC2 stop/start behavior](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/how-ec2-instance-stop-start-works.html),
[EC2 stop methods](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instance-stop-methods.html),
[AWS Go SDK configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html).
