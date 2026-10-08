# Agent-Readable Deployment Diagnostics

Milestone 17 adds two read-only endpoints:

```text
GET /deployments/{id}/logs?tail=100
GET /deployments/{id}/errors
```

`tail` is optional and defaults to 100 lines. Accepted values are 1 through
500. A log response is capped at 64 KiB, and individual messages are capped at
2 KiB. Build output is already capped at 256 KiB when captured; runtime output
is retained as a bounded Docker tail.

Each log entry includes a timestamp, lifecycle stage (`BUILD`, `STARTUP`,
`RUNTIME`, or `HEALTH_CHECK`), stream, and message. Docker runtime log lines use
Docker-provided timestamps. Build and startup records use the deployment's
persisted lifecycle timestamps.

The errors endpoint returns a stable `type` and `stage` alongside timestamp
and message. Current types include:

- `BUILD_FAILURE`
- `DEPENDENCY_INSTALLATION_FAILURE`
- `CONTAINER_STARTUP_FAILURE`
- `APPLICATION_CRASH`
- `HEALTH_CHECK_FAILURE`
- `WORKER_RECOVERY_FAILURE`
- `CLEANUP_FAILURE`

Example:

```json
{
  "deployment_id": "8bb34af2-396c-4b37-8905-1b93c6677a1d",
  "status": "FAILED",
  "errors": [
    {
      "timestamp": "2026-10-08T12:34:56Z",
      "type": "DEPENDENCY_INSTALLATION_FAILURE",
      "stage": "BUILD",
      "message": "docker build failed: ..."
    }
  ]
}
```

These APIs summarize the latest persisted lifecycle failure rather than keeping
an unlimited event history. After changing source, create a new deployment to
preserve the previous deployment's diagnostics for comparison.
