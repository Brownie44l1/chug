# Technical Requirements Document (TRD)
## Image Upload & Processing Service

---

## Proposed Tech Stack

### API Layer — Go with Gin

Go is a statically typed, compiled language that catches bugs before runtime. Gin is a fast, lightweight HTTP framework well suited to I/O-heavy services. Go's concurrency model (goroutines) is excellent for handling many simultaneous upload requests without thread overhead.

**Trade-off accepted:** Go is more verbose than Node.js for simple tasks, but the performance and type safety gains are worth it for a service that must be reliable under load.

### Message Queue — Asynq (backed by Redis)

Asynq is a Go-native queue library backed by Redis. It provides job retries, delays, priorities, and failure handling out of the box. Because the API layer is Go, Asynq is the correct choice — BullMQ is Node.js only and cannot be used here.

**Why not BullMQ:** BullMQ is a Node.js library. Mixing a Go API server with a Node.js queue library would require a bridge process, adding unnecessary complexity.

### Background Worker — Go Worker Process

A standalone Go process that connects to the same Asynq/Redis instance as the API server. Running as a separate process means the worker can crash, restart, or scale independently without affecting API availability.

### Storage — Cloudflare R2

R2 is S3-compatible object storage with no egress fees — meaning reading files back out costs nothing extra. This matters at scale. R2 also has a generous free tier suitable for development and testing (10GB storage, 1M write operations, 10M read operations per month).

**Why not AWS S3:** S3 charges egress fees per GB transferred out. For a service built around image delivery, that cost compounds quickly.

### Database — PostgreSQL

Relational, reliable, and the right tool for structured data with relationships. Job records, developer accounts, API keys, and webhook configurations all have clear relationships that benefit from a relational model and foreign key constraints.

### Webhook Delivery — Part of the Worker (v1)

The worker already knows when a job succeeds or fails. For v1, webhook firing is handled inside the worker process as a non-blocking goroutine. This will be extracted into a dedicated service as the system scales.

---

## Architecture Overview

```
                        DEVELOPER'S WORLD
─────────────────────────────────────────────────────────
  [Developer's App / Backend Server]
         │                        ▲
         │ POST /uploads          │ Webhook callbacks
         │ GET /uploads/:id       │ { job_id, status, url }
         ▼                        │
─────────────────────────────────────────────────────────
                        YOUR SERVICE

  ┌─────────────────────────────────────────────────┐
  │                  API LAYER (Go/Gin)              │
  │                                                  │
  │  1. Authenticate API key (Redis cache            │
  │     → fail closed with 503 if Redis down)        │
  │  2. Validate file (magic bytes, size, checksum)  │
  │  3. Create upload_job record (PostgreSQL)        │
  │  4. Enqueue job (Asynq → Redis)                  │
  │  5. Return 202 Accepted + job_id                 │
  └──────────────────┬──────────────────────────────┘
                     │
                     │ enqueue job
                     ▼
  ┌─────────────────────────────────────────────────┐
  │              REDIS (Asynq Queue)                 │
  │                                                  │
  │  • Holds pending jobs                            │
  │  • Manages retry scheduling                      │
  │  • Caches API key lookups (5 min TTL)            │
  └──────────────────┬──────────────────────────────┘
                     │
                     │ dequeue job
                     ▼
  ┌─────────────────────────────────────────────────┐
  │            BACKGROUND WORKER (Go)                │
  │                                                  │
  │  1. Pick up job from queue                       │
  │  2. Check job is not cancelled (PostgreSQL)      │
  │  3. Check checksum → idempotency guard           │
  │  4. Upload file to R2                            │
  │  5. Update job status (PostgreSQL)               │
  │  6. Fire webhook with exponential backoff        │
  │  7. Log delivery in webhook_deliveries           │
  └──────────────────┬──────────────────────────────┘
                     │
          ┌──────────┴──────────┐
          ▼                     ▼
  ┌──────────────┐    ┌──────────────────────┐
  │  PostgreSQL  │    │   Cloudflare R2       │
  │              │    │                       │
  │  • jobs      │    │  • actual image files │
  │  • developers│    │  • addressed by URL   │
  │  • api_keys  │    │  • deleted by worker  │
  │  • webhooks  │    │    on account cleanup │
  └──────────────┘    └──────────────────────┘

─────────────────────────────────────────────────────────
                    SUPPORTING SYSTEMS

  [TTL Cleanup Cron] — runs periodically, finds jobs
                       where expires_at < now(),
                       deletes records + R2 files in chunks

  [Monitoring / Alerting] — watches Redis health,
                            worker queue depth,
                            webhook failure rates
─────────────────────────────────────────────────────────
```

---

## Request Lifecycle — End to End

```
1. Developer POSTs image to your API
2. API authenticates key (Redis cache hit → fast)
3. API validates file (magic bytes, size, checksum)
4. API writes job record → status: pending (PostgreSQL)
5. API enqueues job (Asynq/Redis) → returns 202 + job_id
   └── Developer's app continues immediately
       User is NOT blocked ✓
6. Worker picks up job → status: processing
7. Worker checks idempotency (checksum in PostgreSQL)
8. Worker uploads to R2
   ├── Success → status: success, store R2 URL
   │            → fire webhook with image URL
   │            → log delivery in webhook_deliveries
   └── Failure → retry if count < max_retries
                → eventually: status: failed
                → fire failure webhook
                → set expires_at (TTL)
```

---

## Alternative Approaches Considered

### Alternative 1 — AWS SQS Instead of Asynq/Redis

| | Asynq + Redis | AWS SQS |
|---|---|---|
| Cost | Cheap, self-hosted | Pay per message |
| Complexity | You manage Redis | Fully managed |
| Features | Rich (priorities, delays, retries, UI) | Basic but reliable |
| Visibility | Full control and observability | Abstracted |
| Best for | Learning, full control | Production at scale |

**Decision:** Asynq + Redis chosen for v1. More to learn from, more control over retry behaviour, easier to inspect locally.

### Alternative 2 — AWS Lambda Instead of Persistent Worker

| | Persistent Worker | AWS Lambda |
|---|---|---|
| Cost | Fixed server cost | Pay per invocation |
| Complexity | Simple to reason about | Cold starts, stateless constraints |
| Retries | Asynq handles automatically | Requires separate retry logic |
| Best for | Predictable workloads | Spiky, unpredictable traffic |

**Decision:** Persistent worker chosen for v1. Lambda introduces cold start latency and stateless constraints that complicate retry logic and local development.

---

## Redis Failure Behaviour

Redis is the backbone of both the queue and the API key cache. Its failure is the most impactful single point of failure in the system.

**Failure mode:** Redis down → auth cache unavailable → service returns `503 Service Unavailable`. The service never fails open. Falling back to PostgreSQL for auth is explicitly rejected — it would transfer the load spike to the database, potentially causing a cascading failure.

**Circuit breaker:** The API server monitors Redis health and trips a circuit breaker on failure, returning `503` immediately rather than allowing requests to pile up and timeout.

**Future mitigation:** Redis Sentinel monitors the primary Redis instance and automatically promotes a replica if the primary dies. Downtime is measured in seconds, not minutes. This is out of scope for v1.

```
Primary Redis ←── Sentinel watches
Replica Redis 1      │
Replica Redis 2 ──────┘ promotes replica if primary dies
```

---

## Scaling Considerations (100k Users)

| Bottleneck | v1 Approach | At Scale |
|---|---|---|
| API layer | Single instance | Multiple instances behind load balancer |
| Worker | Single process | Worker pool — Asynq distributes automatically |
| PostgreSQL | Single instance | Read replicas for status queries |
| Webhook delivery | Inside worker | Dedicated worker + queue |
| Redis | Single instance | Redis Sentinel or Redis Cluster |
| R2 | Managed | Already scales automatically |

---

## Known Weaknesses

**Webhook delivery is the most likely failure point.** The worker fires a webhook to the developer's server, but has no control over that server's availability. A dedicated webhook worker with its own queue and retry schedule is the correct long-term fix. For v1, exponential backoff inside the upload worker is acceptable.

**Redis is a single point of failure for two responsibilities** — the job queue and the auth cache. These should eventually be separated into two Redis instances so a cache failure does not affect queue processing and vice versa.
