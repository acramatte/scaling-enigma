# Local S3 storage and ingestion

This document records the design decisions, setup details, and debugging lessons
behind the project's local S3 ingestion path. It lives next to `s3.go` because
that package is the boundary between the ingestion worker and S3-compatible
storage. The shorter instructions in the root `README.md` remain the normal
getting-started path.

## Why RustFS

The first local implementation used MinIO. During development, the project chose
to move away from MinIO because its open-source support and distribution
direction no longer matched what we wanted for this local dependency. RustFS
was selected as the replacement S3-compatible server.

RustFS is currently pinned in `compose.yaml` rather than using `latest`:

```yaml
image: rustfs/rustfs:1.0.0-beta.10
```

## Current architecture

The local flow is:

```text
S3 client or RustFS console
        |
        | PUT semantic-search/incoming/<object>
        v
RustFS
        |
        | ObjectCreated webhook
        | POST http://semantic-search-ingester:8081/events/s3
        v
Go ingester
        |
        | insert an idempotent ingestion_jobs row
        v
PostgreSQL
        |
        | worker claims pending jobs
        v
RustFS GetObject -> Python embedder -> pgvector document embedding
```

The components have separate responsibilities:

- `compose.yaml` runs PostgreSQL and RustFS.
- `cmd/ingester` exposes the webhook and runs the ingestion worker.
- `internal/ingestion` validates notifications, records jobs, and processes
  them asynchronously.
- `internal/storage/s3.go` uses AWS SDK for Go v2 to fetch objects. A custom
  endpoint enables local RustFS; without one, normal AWS endpoint resolution
  remains available.
- `ingestion_jobs` is the durable application work queue.
- RustFS's webhook queue is a separate delivery queue used before the webhook
  has successfully reached the Go service.

## Bucket and path conventions

There should be exactly one application bucket:

```text
semantic-search
```

Only object-created events under this prefix are sent to the ingester:

```text
incoming/
```

For example:

```text
s3://semantic-search/incoming/drumkit.jpg
```

The resulting document uses the same S3 URI as its source URI.

Do not create an `events` bucket. An `events` bucket appeared during debugging
because the RustFS webhook queue was initially placed at `/data/events`.
RustFS treats ordinary top-level directories under `/data` as buckets, so the
queue directory leaked into the S3 namespace.

The queue now uses:

```text
/data/.rustfs-events
```

This location is deliberate:

- `/data` belongs to RustFS's unprivileged runtime user;
- the directory is persisted by the `rustfs_data` volume;
- the leading dot keeps this internal state out of the S3 bucket listing.

Do not move the queue to a new mounted directory without checking ownership.
RustFS runs as UID/GID 10001. A Docker named volume mounted at `/events` was
owned by `root:root` with mode `0755`, which prevented RustFS from opening its
queue store.

## Why `storage-cli` exists

The RustFS image runs the server but does not provide the `mc` commands used to
create buckets and manage bucket notification rules. `compose.yaml` therefore
contains a profile-gated `storage-cli` service based on `minio/mc`.

It is not a server or long-running initialization container. The Makefile runs
it as a disposable client:

```text
docker compose run --rm --no-deps --entrypoint /bin/sh storage-cli ...
```

`--rm` removes each helper container when the command finishes. The Compose
profile also keeps the helper out of a normal `docker compose up`.

Using this helper avoids requiring every developer to install `mc` on the host.
It shares host networking with RustFS and connects to `127.0.0.1:9000`.

## Why configuration order matters

A bucket notification rule is only a reference to a live notification target.
RustFS beta.10 can load persisted bucket rules before its persisted notification
switch is available, leaving a visible rule whose runtime target is absent.

Compose therefore defines the `primary` target through environment variables,
and the RustFS command creates `/data/.rustfs-events` before server startup.
`make storage-configure` then performs these operations in order:

1. Re-enable/reload the notification module through the signed admin API.
2. Require the `primary` webhook target to report `online` at runtime.
3. Create the `semantic-search` bucket if it does not exist.
4. Replace the bucket notification rule so the runtime rule engine is populated.

The relevant target is:

```text
arn:rustfs:sqs:us-east-1:primary:webhook
```

The webhook target points to:

```text
http://semantic-search-ingester:<INGESTION_PORT>/events/s3
```

RustFS runs inside Docker while the Go ingester currently runs on the Linux
host. RustFS uses host networking, and `semantic-search-ingester` is mapped to
`127.0.0.1` inside the container. This avoids host firewall rules that silently
drop Docker bridge traffic.

RustFS beta.10 rejects literal loopback and private-IP webhook URLs before
creating a target, so the configured URL must use the local hostname rather
than `127.0.0.1`. Start the ingester before running `make storage-up`.

## Authentication

`INGESTION_WEBHOOK_TOKEN` protects `POST /events/s3`. Compose configures RustFS
to send it as a bearer authorization value. The receiver accepts the configured
token in the authorization header and rejects a mismatch with HTTP 401. Keep
the variable set whenever the listener is reachable by anything other than the
local development process; the current receiver accepts unauthenticated events
when no token is configured.

Local credentials belong in `.env`, based on `.env.example`. Do not commit real
S3 credentials or webhook tokens. The main variables are:

```text
S3_ENDPOINT
S3_REGION
S3_ACCESS_KEY
S3_SECRET_KEY
S3_BUCKET
S3_PORT
S3_CONSOLE_PORT
INGESTION_ADDR
INGESTION_PORT
INGESTION_WEBHOOK_TOKEN
```

`INGESTION_ADDR` controls where Go listens, while `INGESTION_PORT` is inserted
into RustFS's webhook URL. Their port numbers must agree. With the documented
defaults, they are `0.0.0.0:8081` and `8081` respectively.

`S3_ENDPOINT` enables path-style addressing for RustFS. If it is unset, the Go
storage client uses the AWS SDK's normal endpoint and credential resolution.
For custom endpoints, the client also sets response-checksum validation to
`when_required`. RustFS beta.10 does not return optional `x-amz-checksum-*`
headers on `GetObject`; the SDK's `when_supported` default would otherwise log
`Response has no supported checksum` for every successful read. AWS S3 keeps
the SDK's default checksum behavior because this override applies only when
`S3_ENDPOINT` is configured.

## Local runbook

Start the database and apply migrations:

```bash
make db-up
make migrate-up
```

Start the embedder and ingester in separate terminals:

```bash
python embed_service.py
go run ./cmd/ingester
```

After the ingester is healthy, start and configure RustFS:

```bash
make storage-up
```

Inspect the notification rule:

```bash
make storage-status
```

The status command must report the `primary` webhook target as `online`.
`make storage-events` lists only persisted bucket rules and is not a runtime
health check.

Upload an image under `incoming/` through the RustFS console at
`http://127.0.0.1:9001`, or use any S3-compatible client. The S3 API is exposed
at `http://127.0.0.1:9000` by default.

Service lifecycle commands are intentionally scoped:

```bash
make db-down       # remove only the PostgreSQL container; keep its volume
make storage-down  # remove only the RustFS container; keep its volume
make stack-down    # remove the complete Compose stack; keep named volumes
```

`make storage-configure` is a repair/reapply command. Normal startup should use
`make storage-up`, which waits for RustFS and invokes configuration itself.

## Idempotency and processing behavior

The webhook handler returns promptly after recording jobs. Object download and
embedding happen in the worker, not in the webhook request.

Jobs are unique by bucket, object key, object version, and ETag. Repeated event
delivery therefore does not create duplicate work for the same object state.
Transient storage or embedder failures are retried up to three attempts with a
short backoff before the job is marked `failed`.

Terminal failed jobs remain part of that identity and are not silently reset by
a duplicate notification. After correcting the recorded root cause, explicitly
give the job a fresh retry budget:

```bash
make ingestion-retry JOB_ID=<failed-job-id>
```

The operation accepts only a `failed` job. It resets processing timestamps,
attempt count, and the previous error while preserving the original object
identity and job record. It refuses pending, processing, completed, or ignored
jobs so it cannot race active work.

Receiver logs report an object as `inserted` when PostgreSQL created a new job
and `duplicate` when the same bucket, key, version, and ETag already existed.
Both outcomes mean webhook delivery itself succeeded.

Only image ingestion is implemented currently. Non-image uploads, including
videos, are recorded and then marked `ignored` with the reason:

```text
only image ingestion is implemented
```

This is intentional rather than a webhook failure.

## What failed during the initial setup

The original symptom was that uploads did not appear to reach the Go webhook.
The receiver itself was functioning. RustFS logs exposed the actual failure:

```text
Failed to open store for Webhook target: I/O error: Permission denied
```

Because the queue store could not be opened, RustFS skipped the target. Bucket
notification registration then failed with:

```text
No notify targets configured
```

The failed sequence was:

1. mount a new named volume at `/events`;
2. Docker initializes it as `root:root 0755`;
3. RustFS runs as UID/GID 10001 and cannot write there;
4. target creation is skipped;
5. the bucket rule references a target that does not exist;
6. no events can be dispatched.

Moving the queue to writable persistent storage fixed target creation. Hiding it
under `/data/.rustfs-events` also prevented internal queue state from appearing
as an S3 bucket.

A later failure exposed two additional beta.10 behaviors:

- persisted bucket rules can remain visible even when their runtime target was
  not initialized during startup;
- literal loopback and private-IP webhook URLs are rejected by RustFS's outbound
  URL policy.

The current setup defines the target through environment variables, reloads the
notification module after RustFS is healthy, verifies that the target is online,
and then reapplies the bucket rule. Linux host networking plus the mapped local
hostname avoids both Docker bridge firewall drops and the literal-IP rejection.

An end-to-end probe then confirmed the complete boundary chain: a real object
PUT caused RustFS to POST the event, the Go receiver inserted an ingestion job,
and the worker claimed it. Directly curling the Go endpoint or merely listing a
bucket rule would not have proven that complete path.

## Troubleshooting checklist

When events stop arriving, check each boundary independently.

1. Confirm the ingester is listening:

   ```bash
   curl --fail http://127.0.0.1:8081/health
   ```

2. Confirm RustFS is healthy:

   ```bash
   docker compose ps
   ```

3. Confirm the runtime target is online and the bucket rule exists:

   ```bash
   make storage-status
   ```

   A listed rule with an `offline` or absent target is not operational.

4. Confirm only the intended bucket exists:

   ```bash
   docker compose run --rm --no-deps --entrypoint /bin/sh storage-cli -ec \
     'mc alias set local "$S3_ENDPOINT" "$S3_ACCESS_KEY" "$S3_SECRET_KEY" >/dev/null; mc ls local'
   ```

5. Check queue ownership:

   ```bash
   docker compose exec rustfs stat -c '%U:%G %a %n' /data/.rustfs-events
   ```

   It should be owned by `rustfs:rustfs` and writable by that user.

6. Inspect RustFS logs for target initialization or delivery errors:

   ```bash
   docker compose logs rustfs
   ```

7. Inspect recent application jobs:

   ```sql
   SELECT id, object_key, status, attempts, last_error, updated_at
   FROM ingestion_jobs
   ORDER BY id DESC
   LIMIT 20;
   ```

8. Finish with a real object upload under `incoming/`. Clean up any diagnostic
   object and job afterward.

Useful interpretations:

- `401 Unauthorized`: RustFS and the ingester disagree on the webhook token.
- Connection refused or timeout: the ingester is not listening or RustFS was
  started without the expected Linux host-network configuration.
- Target status `offline`: inspect the configured endpoint and remember that
  beta.10 rejects literal loopback/private IPs even when `curl` can reach them.
- `No notify targets configured`: target registration failed before the bucket
  rule was applied.
- Queue-store permission errors: check the runtime UID and directory ownership.
- A job remains `pending`: webhook delivery worked; investigate the worker.
- A job is `ignored`: inspect its media type; non-images are currently expected
  to be ignored.
- A job is `failed`: inspect `attempts` and `last_error` for S3, embedder, or
  database errors.
- Re-uploading identical bytes to the same key has the same ETag and is
  deduplicated against an existing job, including a terminal failed job. Use a
  new key while testing or run `make ingestion-retry JOB_ID=<id>` after fixing
  the failure.

## Persistent data and cleanup

With the default Compose project name, the active named volume is:

```text
semantic-search_rustfs_data
```

The prefix changes if Compose is run with another project name. Use
`docker volume ls` to identify the actual volume before any manual cleanup.

It contains application objects, RustFS internal state, notification
configuration, and the hidden webhook queue. Removing it resets local object
storage completely.

The old MinIO data and event volumes were deliberately removed after confirming
that no development objects needed migration. There is no automatic MinIO to
RustFS migration path in this project.

Disposable diagnostic containers and RustFS beta.8/beta.9 images should not be
kept after troubleshooting. Avoid broad Docker volume pruning commands because
they can remove unrelated project data.

## Current limitations and future work

- RustFS is a local-development dependency; production can use an S3-compatible
  service or AWS S3 through the same Go storage boundary.
- The RustFS version is still a beta and intentionally pinned. Review release
  notes and rerun the end-to-end event probe before upgrading it.
- The current local webhook path uses Linux host networking. If the ingester
  moves into Compose, put both services on the same Compose network and use the
  ingester service name instead.
- Claimed jobs do not yet have a processing lease. If the ingester process exits
  after a job changes to `processing`, that job must be inspected and recovered
  deliberately rather than being reclaimed automatically.
- Image objects are currently buffered in memory before being sent to the
  embedder, without a configured maximum object size. The worker also does not
  yet use the event ETag as an `If-Match` precondition when fetching an
  unversioned object. Add both safeguards before accepting untrusted or
  high-volume uploads.
- Video ingestion is not implemented yet. It is expected to extract frames and
  produce segment-aware embeddings in a later change.
- Local HTTP endpoints and development credentials are not production security
  settings. Production deployment requires appropriate TLS, secret management,
  network policy, and S3 permissions.
