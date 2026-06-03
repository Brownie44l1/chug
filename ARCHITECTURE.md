# Architecture Document
## Image Upload & Processing Service

---

## System Overview

The service is composed of four runtime components — an API server, a message queue, a background worker, and supporting infrastructure. Each component has a single responsibility and communicates with the others through well-defined boundaries.

```
                        DEVELOPER'S WORLD
─────────────────────────────────────────────────────────────
  [Developer's App / Backend Server]
         │                              ▲
         │ HTTP Requests                │ Webhook Callbacks
         │ POST /uploads                │ { job_id, status, url }
         │ GET  /uploads/:job_id        │
         │ POST /uploads/:job_id/retry  │
         ▼                              │
─────────────────────────────────────────────────────────────
                        YOUR SERVICE

  ┌──────────────────────────────────────────────────────┐
  │                   API LAYER (Go/Gin)                  │
  │                                                       │
  │   → Authenticate API key                             │
  │     (Redis cache hit → fast; miss → fail closed 503) │
  │   → Validate file                                    │
  │     (magic bytes, size limit, checksum)              │
  │   → Write upload_job record (PostgreSQL)             │
  │   → Enqueue job payload (Asynq → Redis)              │
  │   → Return 202 Accepted + job_id immediately         │
  └─────────────────────┬────────────────────────────────┘
                        │
                        │ enqueue
                        ▼
  ┌──────────────────────────────────────────────────────┐
  │                 REDIS (Asynq Queue)                   │
  │                                                       │
  │   • Holds pending upload jobs                        │
  │   • Manages retry scheduling and backoff             │
  │   • Caches API key lookups (5 min TTL)               │
  │   • Single point of failure — circuit breaker        │
  │     trips on failure, returns 503 to all requests    │
  └─────────────────────┬────────────────────────────────┘
                        │
                        │ dequeue
                        ▼
  ┌──────────────────────────────────────────────────────┐
  │              BACKGROUND WORKER (Go)                   │
  │                                                       │
  │   → Fetch job details from PostgreSQL by job_id      │
  │   → Check job status is not cancelled                │
  │   → Check checksum for idempotency                   │
  │   → Upload file bytes to Cloudflare R2               │
  │   → Update job status in PostgreSQL                  │
  │   → Fire webhook (non-blocking goroutine)            │
  │   → Log delivery attempt in webhook_deliveries       │
  └──────────┬──────────────────────┬────────────────────┘
             │                      │
             ▼                      ▼
  ┌────────────────────┐  ┌─────────────────────────────┐
  │    PostgreSQL      │  │       Cloudflare R2          │
  │                    │  │                              │
  │  developers        │  │  Stores actual image files   │
  │  api_keys          │  │  Addressed by public URL     │
  │  developer_settings│  │  Deleted by worker on        │
  │  upload_jobs       │  │  account cleanup             │
  │  webhook_endpoints │  │                              │
  │  webhook_deliveries│  │                              │
  └────────────────────┘  └─────────────────────────────┘

─────────────────────────────────────────────────────────────
                      SUPPORTING SYSTEMS

  [TTL Cleanup Cron]
    Runs on a schedule. Finds upload_jobs where
    expires_at < now() and status = failed.
    Deletes R2 files in chunks with checkpointing,
    then removes database records.

  [Monitoring & Alerting]
    Watches Redis health, worker queue depth,
    job failure rates, and webhook delivery failures.
    Pages on-call engineer if Redis goes down or
    queue depth exceeds threshold.
─────────────────────────────────────────────────────────────
```

---

## Component Responsibilities

| Component | Responsibility | Must NOT Do |
|---|---|---|
| API Server | Accept, validate, acknowledge requests | Process or upload files |
| Redis | Hold jobs, cache auth lookups | Be the source of truth for job data |
| Worker | Process jobs, upload to R2, fire webhooks | Accept HTTP requests |
| PostgreSQL | Persist all structured data | Cache hot-path lookups |
| Cloudflare R2 | Store image files | Store metadata or job state |

---

## Request Lifecycle — Full Walkthrough

### Happy Path

```
Developer sends POST /uploads with image + checksum + API key
          │
          ▼
API: validate API key → Redis cache hit (fast path)
          │
          ▼
API: validate file
  → read magic bytes → confirm JPEG / PNG / WEBP / GIF
  → check file size ≤ 10MB
  → verify checksum matches file
          │
          ▼
API: INSERT upload_job (status: pending) → PostgreSQL
          │
          ▼
API: enqueue job_id → Asynq / Redis
          │
          ▼
API: return 202 Accepted + { job_id }
          │
          │    ← Developer's app is free to continue here
          │
          ▼
Worker: dequeue job_id from Asynq
          │
          ▼
Worker: SELECT upload_job WHERE id = job_id → PostgreSQL
  → check status ≠ cancelled
  → check checksum not already in R2 (idempotency)
          │
          ▼
Worker: UPDATE upload_job SET status = processing
          │
          ▼
Worker: upload file bytes → Cloudflare R2
          │
          ▼
Worker: UPDATE upload_job SET status = success, storage_url = <r2_url>
          │
          ▼
Worker: fire webhook → POST developer's registered URL
  → payload: { job_id, status: "success", storage_url }
  → signed with HMAC secret
          │
          ▼
Worker: INSERT webhook_delivery log
```

### Failure Path

```
Worker: upload to R2 fails
          │
          ▼
Asynq: retry with exponential backoff
  → Attempt 1: immediately
  → Attempt 2: 30 seconds
  → Attempt 3: 5 minutes
  → Attempt 4: 30 minutes
          │
          ▼
All retries exhausted:
  → UPDATE upload_job SET status = failed,
      failure_reason = <error>, expires_at = now() + 24hrs
          │
          ▼
Worker: fire failure webhook
  → payload: { job_id, status: "failed", failure_reason }
          │
          ▼
Developer calls POST /uploads/:job_id/retry within 24hrs
  → job reset to pending, retry_count = 0
  → re-enqueued to Asynq
          │
          ▼
TTL Cleanup Cron (runs periodically):
  → finds jobs where expires_at < now()
  → deletes R2 files in chunks with checkpointing
  → removes database records
```

---

## Failure Analysis

### Redis Goes Down

```
Redis unavailable
      │
      ├── API key cache unavailable
      │     └── Circuit breaker trips
      │         → All requests return 503 immediately
      │         → No fallback to PostgreSQL (fail closed)
      │
      └── Job queue unavailable
            → New jobs cannot be enqueued
            → Workers go idle (queue is empty)
            → In-flight jobs already picked up complete normally
            → Developer experiences 503 on all upload attempts
```

**Developer experience:** Upload endpoint returns 503. The developer's user flow is not blocked — it receives a clear error and can retry or surface a message to their user.

**Recovery:** Redis restarts → circuit breaker resets → queue resumes → workers pick up backlog. No data is lost because job records were written to PostgreSQL before enqueue was attempted.

### Worker Crashes Mid-Upload

```
Worker picks up job → starts uploading to R2 → crashes
      │
      ▼
Asynq: job was never acknowledged → returns to queue
      │
      ▼
Worker restarts → picks up same job again
      │
      ▼
Idempotency check: was this checksum already uploaded?
  → Yes: skip R2 upload, use existing URL, mark success
  → No:  upload proceeds normally
```

**Result:** No duplicate files. No data loss. The checksum-based idempotency guard makes retries safe.

### Developer's Webhook Server Is Down

```
Worker fires webhook → developer server returns 500 or times out
      │
      ▼
Retry with exponential backoff:
  30s → 5min → 30min → 1hr
      │
      ▼
All attempts exhausted
  → webhook_delivery marked failed
  → job record status is unaffected (still success)
  → developer can query GET /uploads/:job_id to check status
```

---

## Weakest Points in This Architecture

### 1. Webhook Delivery (Highest Risk)

Webhook delivery runs inside the upload worker as a goroutine. If the developer's server is unreliable, the worker spends time on retries that could be used processing new jobs. At scale, this becomes a resource contention problem.

**Long-term fix:** Extract webhook delivery into a dedicated worker with its own Asynq queue. Upload jobs and webhook jobs never compete for worker resources.

### 2. Redis as a Single Point of Failure

Redis handles two responsibilities — the job queue and the auth cache. A single Redis failure takes down both simultaneously. These should eventually be separated into two Redis instances.

**Long-term fix:** Redis Sentinel for high availability. Two Redis instances — one for queue, one for cache — so failures are isolated.

### 3. Single Worker Process

One worker processes all jobs sequentially. A spike in uploads creates a growing backlog with no way to catch up.

**Long-term fix:** Run a pool of worker instances. Asynq distributes jobs across workers automatically with no code changes required.

---

## Scaling Roadmap

| Stage | Users | Change |
|---|---|---|
| v1 | < 1k developers | Single API instance, single worker, single Redis, single PostgreSQL |
| v2 | 1k–10k developers | Worker pool (3–5 instances), Redis Sentinel, dedicated webhook worker |
| v3 | 10k–100k developers | API load balancer, PostgreSQL read replicas, Redis split (queue vs cache) |
| v4 | 100k+ developers | Managed queue (AWS SQS), multi-region R2, observability platform |
