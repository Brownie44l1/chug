# Epics & Tickets
## Image Upload & Processing Service

---

## Overview

| Epic | Title | Tickets | Priority |
|---|---|---|---|
| Epic 1 | Project Foundation & Auth | 1.1 – 1.5 | Week 1 |
| Epic 2 | Core Upload Flow | 2.1 – 2.4 | Week 1 |
| Epic 3 | Background Worker & Storage | 3.1 – 3.4 | Week 1–2 |
| Epic 4 | Webhook Delivery | 4.1 – 4.3 | Week 2+ |

---

## Week 1 Critical Path

```
1.1 → 1.2 → 1.4 → 2.1 → 2.2 → 2.3 → 3.1 → 3.2 → 2.4
```

Every ticket in this path feeds the next. Removing any single one breaks the demo. Tickets outside this path (1.3, 1.5, 3.3, 3.4, all of Epic 4) are week 2 and beyond.

---

## MVP Scope (5-Day Demo)

| Ticket | Title | Include in Demo? | Reason |
|---|---|---|---|
| 1.1 | Project Setup | Yes | Foundation — nothing runs without it |
| 1.2 | Database Schema | Yes | Required for job records |
| 1.3 | Developer Registration | No | Hardcode one API key in .env instead |
| 1.4 | Auth Middleware | Yes (simplified) | Security boundary — never skip auth |
| 1.5 | Rate Limiting | No | No abuse risk on day 1 |
| 2.1 | File Validation | Yes | Core security gate |
| 2.2 | Upload Job Creation | Yes | Core upload flow |
| 2.3 | Queue Integration | Yes | Core async promise |
| 2.4 | Job Status Endpoint | Yes | Only way to check results without webhooks |
| 3.1 | Worker Setup | Yes | Nothing processes without it |
| 3.2 | R2 Upload | Yes | Files must go somewhere |
| 3.3 | Retry & Failure Handling | No | Happy path only for demo |
| 3.4 | Manual Retry Endpoint | No | Requires 3.3 first |
| 4.1 | Webhook Registration | No | Developer polls instead |
| 4.2 | Webhook Firing | No | Developer polls instead |
| 4.3 | Webhook Retry | No | Requires 4.2 first |

> **Auth simplification for demo:** Instead of a full registration flow (Ticket 1.3), set one API key in the `.env` file and have the middleware validate against it. This keeps auth in place without the overhead of a registration system.

---

## Epic 1 — Project Foundation & Auth

*Get the skeleton standing and secure before anything else.*

---

### Ticket 1.1 — Project Setup
**Priority:** Must have — foundation ticket. Nothing else can be built or tested without it.

Set up Go project structure with Gin, connect to PostgreSQL and Redis, and establish environment variable configuration.

**Acceptance Criteria**
- Server starts and returns `200 OK` on `GET /health`
- PostgreSQL connection verified on startup; server exits with a clear error if connection fails
- Redis connection verified on startup; server exits with a clear error if connection fails
- Environment variables loaded from `.env` file using a config package
- Project structure separates concerns: `/handlers`, `/services`, `/workers`, `/models`, `/config`
- A `Makefile` exists with targets for `run`, `test`, and `migrate`

---

### Ticket 1.2 — Database Schema Migration
**Priority:** Must have — no persistence without it.

Write and run versioned migrations for all core tables.

**Acceptance Criteria**
- `developers` table created with correct columns and constraints
- `api_keys` table created with foreign key to `developers`
- `developer_settings` table created with foreign key to `developers`
- `upload_jobs` table created with correct ENUM values and indexes
- `webhook_endpoints` table created with foreign key to `developers`
- `webhook_deliveries` table created with foreign keys to `upload_jobs` and `webhook_endpoints`
- All recommended indexes from the ERD are applied
- Migrations are versioned and repeatable (tool: `golang-migrate` or `goose`)
- A rollback migration exists for every forward migration

---

### Ticket 1.3 — Developer Registration & API Key Issuance
**Priority:** Week 2 — hardcode one key in `.env` for the demo.

Allow a developer to create an account and receive an API key.

**Acceptance Criteria**
- `POST /auth/register` creates a developer record and returns a generated API key
- Password is hashed using bcrypt before storage — raw password is never stored
- Generated API key is hashed in the database; raw key is returned exactly once and never again
- `POST /auth/keys` creates an additional API key with a required `label` field
- `DELETE /auth/keys/:id` sets `is_active = false` and records `revoked_at`
- A revoked key cannot be reactivated — only replaced
- Registration returns `409 Conflict` if email is already registered

---

### Ticket 1.4 — API Key Authentication Middleware
**Priority:** Must have — every protected route depends on it.

Every protected route must verify the API key before the request proceeds.

**Acceptance Criteria**
- Missing `Authorization` header returns `401 Unauthorized` with a descriptive error message
- Invalid or unrecognised key returns `401 Unauthorized`
- Revoked key (`is_active = false`) returns `401 Unauthorized`
- Valid key attaches developer context (developer_id, api_key_id) to the request for downstream use
- Valid key lookups are cached in Redis with a 5-minute TTL
- If Redis is unavailable, middleware returns `503 Service Unavailable` — it never falls back to direct PostgreSQL lookup
- Middleware is applied to all routes except `GET /health` and `POST /auth/register`

---

### Ticket 1.5 — Rate Limiting Per API Key
**Priority:** Week 2 — no abuse risk during demo.

Prevent any single API key from overwhelming the service.

**Acceptance Criteria**
- Each API key is limited to 100 requests per minute
- Exceeding the limit returns `429 Too Many Requests`
- Response includes a `Retry-After` header indicating when the client may retry
- Rate limit counters are stored in Redis using a sliding window algorithm
- Each API key has an independent counter — one key's limit does not affect others
- Rate limit configuration (requests per window, window duration) is set via environment variable

---

## Epic 2 — Core Upload Flow

*The heart of the system. Upload accepted, validated, queued.*

---

### Ticket 2.1 — File Validation Service
**Priority:** Must have — security and data integrity gate.

Validate every incoming file before it touches the queue or the database.

**Acceptance Criteria**
- Reads the first bytes of the file to verify real file type (magic bytes), not the extension
- Accepts only JPEG (`FF D8 FF`), PNG (`89 50 4E 47`), WEBP, and GIF
- Rejects files exceeding the developer's configured `max_file_size_bytes` (default 10MB)
- Checks file size before reading the full file into memory — fail fast on oversized files
- Verifies developer-provided checksum matches the computed checksum of the file
- Each validation failure returns a specific `400 Bad Request` with an error code and message:
  - `INVALID_FILE_TYPE`
  - `FILE_TOO_LARGE`
  - `CHECKSUM_MISMATCH`
- Validation logic lives in a standalone service package — not inside the HTTP handler

---

### Ticket 2.2 — Upload Job Creation
**Priority:** Must have — core upload endpoint.

Accept a validated upload, persist a job record, and acknowledge immediately.

**Acceptance Criteria**
- `POST /uploads` requires a valid API key (enforced by middleware from Ticket 1.4)
- File passes validation before any database writes occur
- Creates an `upload_job` record with `status: pending`
- Returns `202 Accepted` with `{ job_id }` — response time must be under 500ms regardless of file size
- `job_id` is a UUID v4 — not sequential (sequential IDs expose enumeration attacks)
- If database write succeeds but queue enqueue fails, job is marked `failed` and `503` is returned
- Request body is multipart form: `file` (binary) + `checksum` (string)

---

### Ticket 2.3 — Asynq Queue Integration
**Priority:** Must have — without this nothing is async.

Enqueue the job to the background worker reliably after the job record is created.

**Acceptance Criteria**
- Job is enqueued to Asynq immediately after the database record is created
- Job payload contains `job_id` only — the worker fetches full details from PostgreSQL
- If enqueue fails, the job record is updated to `status: failed` and the API returns `503`
- Queue name is configurable via environment variable (e.g. `uploads:default`)
- Enqueue operation is wrapped in a timeout — does not block indefinitely if Redis is slow

---

### Ticket 2.4 — Job Status Endpoint
**Priority:** Must have — only way to check results without webhooks in v1.

Let developers query the current state of any upload job.

**Acceptance Criteria**
- `GET /uploads/:job_id` returns the current job status
- Response includes `storage_url` when `status = success`
- Response includes `failure_reason` when `status = failed`
- Response includes `expires_at` when `status = failed` so developer knows the retry window
- Returns `404 Not Found` for an unrecognised `job_id`
- Returns `403 Forbidden` if the job belongs to a different developer
- Response shape is consistent regardless of status — missing fields are `null`, not omitted

---

## Epic 3 — Background Worker & Storage

*Where the actual work happens.*

---

### Ticket 3.1 — Worker Process Setup
**Priority:** Must have — nothing processes without it.

A standalone Go process that connects to Asynq and is ready to process jobs.

**Acceptance Criteria**
- Worker starts as an independent process, separate from the API server
- Connects to the same Redis instance as the API server via environment variable config
- Worker concurrency (number of simultaneous jobs) is configurable via environment variable
- Graceful shutdown: worker finishes its current job before stopping — it does not drop in-flight work
- Every job processed is logged with: `job_id`, `status`, `duration_ms`, `error` (if any)
- Worker reconnects automatically if Redis connection is temporarily lost

---

### Ticket 3.2 — R2 Upload Integration
**Priority:** Must have — files must go somewhere.

Pick up jobs from the queue, upload files to Cloudflare R2, and update job status.

**Acceptance Criteria**
- Worker fetches full job details from PostgreSQL using `job_id` from the queue payload
- Checks `status ≠ cancelled` before doing any work — skips cancelled jobs silently
- Checks idempotency: if a file with the same checksum already has a `storage_url` in PostgreSQL, skip the R2 upload and reuse the existing URL
- Uploads file bytes to R2 with the correct `Content-Type` header derived from the validated `mime_type`
- On success: updates `upload_job` with `status: success` and `storage_url`
- On R2 error: returns the error to Asynq so the retry mechanism in Ticket 3.3 can handle it
- R2 bucket name and credentials are loaded from environment variables — never hardcoded

---

### Ticket 3.3 — Retry & Failure Handling
**Priority:** Week 2 — happy path only for the demo.

Handle upload failures gracefully with automatic retries and permanent failure recording.

**Acceptance Criteria**
- Failed upload is retried automatically by Asynq up to `max_retries` times (default: 3)
- Retries follow exponential backoff: 30s → 5min → 30min
- `retry_count` on the job record is incremented on each attempt
- After all retries are exhausted, job is updated: `status: failed`, `failure_reason` populated, `expires_at = now() + ttl_hours`
- Failure reason is human-readable (e.g. `"R2 upload failed: connection timeout"`)
- A scheduled cron job runs every hour, finds jobs where `expires_at < now()` and `status = failed`, and deletes them in chunks of 100 with a checkpoint after each chunk

---

### Ticket 3.4 — Manual Retry Endpoint
**Priority:** Week 2 — requires Ticket 3.3 to be meaningful.

Let developers manually retry a permanently failed job within its TTL window.

**Acceptance Criteria**
- `POST /uploads/:job_id/retry` is accepted only for jobs with `status: failed`
- Returns `404 Not Found` if the job does not exist or `expires_at` has passed
- Returns `403 Forbidden` if the job belongs to a different developer
- Returns `400 Bad Request` if `status ≠ failed` (e.g. trying to retry a successful job)
- On valid retry: resets `retry_count = 0`, `status = pending`, clears `failure_reason` and `expires_at`
- Re-enqueues the job to Asynq
- Returns `202 Accepted` with the same `job_id`

---

## Epic 4 — Webhook Delivery

*Notify developers so they don't have to poll.*

---

### Ticket 4.1 — Webhook Registration
**Priority:** Week 2 — developer can poll via Ticket 2.4 in the meantime.

Let developers register a URL to receive job notifications.

**Acceptance Criteria**
- `POST /webhooks` registers a URL for the authenticated developer
- Validates the URL is a valid HTTPS address before saving
- Generates a random HMAC signing secret per endpoint
- Returns the signing secret exactly once in the registration response — it is never returned again
- `GET /webhooks` returns all registered endpoints for the developer (secret is never included)
- `DELETE /webhooks/:id` sets `is_active = false` — does not hard delete the record
- A developer can register multiple webhook endpoints

---

### Ticket 4.2 — Webhook Firing on Job Completion
**Priority:** Week 2 — requires Ticket 4.1.

Notify the developer when a job reaches a terminal status (success or failed).

**Acceptance Criteria**
- Worker fires a webhook after every job reaches `success` or `failed` status
- Webhook is fired as a non-blocking goroutine — it does not delay job completion
- Payload includes: `job_id`, `status`, `storage_url` (on success), `failure_reason` (on failure), `timestamp`
- Payload is signed: `X-Webhook-Signature` header contains an HMAC-SHA256 signature of the payload body using the endpoint secret
- A `webhook_delivery` record is created before the HTTP request is made, with `status: pending`
- Delivery record is updated to `delivered` or `failed` based on the response

---

### Ticket 4.3 — Webhook Retry with Exponential Backoff
**Priority:** Week 2 — requires Ticket 4.2.

Handle unresponsive or temporarily unavailable developer webhook servers.

**Acceptance Criteria**
- Any non-`2xx` response or request timeout triggers a retry
- Retries follow exponential backoff: 30s → 5min → 30min → 1hr
- After 4 failed attempts, `webhook_delivery` is marked `failed` — no further retries
- A failed webhook delivery does NOT change the `upload_job` status — the job is still `success` or `failed` regardless
- `attempt_count` and `last_attempted_at` are updated on every attempt
- Developer can query `GET /webhooks/deliveries/:job_id` to inspect delivery history for a specific job

---

## Epic 5 — Parking Lot

*Deferred features, architectural improvements, and future scope.*

---

### Ticket 5.1 — Developer Registration & API Key Issuance (Deferred Ticket 1.3)
**Priority:** Low / Week 2+
Implement full self-service registration and multiple key management endpoints (`POST /auth/register`, `POST /auth/keys`, `DELETE /auth/keys/:id`). Currently bypassed for demo mode in favor of a pre-configured `.env` API key.

### Ticket 5.2 — Rate Limiting Per API Key (Deferred Ticket 1.5)
**Priority:** Low / Week 2+
Protect the API endpoints from abuse using a sliding window rate limiter in Redis (100 requests per minute per key) returning `429 Too Many Requests`.

### Ticket 5.3 — Webhook Worker Graceful Shutdown & Work Queue Integration
**Priority:** Medium / Recommended Improvement
Currently, webhooks are fired as raw in-flight background goroutines. If the worker crashes or restarts, these in-flight webhooks can be lost without update. Firing webhooks should utilize a dedicated Asynq queue or register wait groups to block shutdown until all in-flight requests finish cleanly.
