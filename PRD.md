# Product Requirements Document (PRD)
## Image Upload & Processing Service

---

## Problem Statement

Developers building user-facing apps need a reliable way to handle image uploads without blocking critical user flows. Network failures, timeouts, and partial uploads are hard problems that shouldn't be reimplemented in every app.

---

## One-Liner

> This API service handles image uploads asynchronously for developers so that slow networks or upload failures never block time-sensitive user actions like registration.

---

## Target User

Backend developers who need to offload reliable image upload handling from their own servers.

---

## What We Are Building

A backend API service that:
- Accepts image uploads from a developer's server
- Handles storage reliably with automatic retries
- Notifies the developer asynchronously via webhooks when an upload succeeds or fails permanently
- Exposes job status endpoints so developers can poll for results without webhooks

---

## Core User Flows (Happy Path Only)

### Flow 1 — Fire and Forget Upload

```
Developer server receives image from their user
→ Developer calls POST /uploads on our API
→ Our API returns job_id immediately (202 Accepted)
→ Our service processes and stores the image async
→ On success, webhook fires to developer's endpoint
→ Developer updates their DB with the image URL
```

### Flow 2 — Handling a Failed Upload

```
Upload fails after all retries exhausted
→ Our service marks job as `failed` with a TTL of 24hrs
→ Webhook fires to developer → { job_id, status: "failed", reason }
→ Developer calls POST /uploads/:job_id/retry within 24hrs
→ Flow restarts from processing
→ After TTL expires → job record deleted automatically
```

### Flow 3 — Developer Checks Upload Status

```
Developer calls GET /uploads/:job_id
→ Returns current status: pending | processing | success | failed | cancelled
→ Allows developer to build their own polling if needed
```

---

## API Contract

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/auth/register` | Create a developer account |
| POST | `/auth/keys` | Issue a new API key |
| DELETE | `/auth/keys/:id` | Revoke an API key |
| POST | `/uploads` | Submit a new image upload job |
| GET | `/uploads/:job_id` | Get status of an upload job |
| POST | `/uploads/:job_id/retry` | Retry a failed job within TTL |
| POST | `/webhooks` | Register a webhook endpoint |
| GET | `/webhooks` | List registered webhook endpoints |
| DELETE | `/webhooks/:id` | Deactivate a webhook endpoint |
| GET | `/webhooks/deliveries/:job_id` | Check webhook delivery status for a job |
| GET | `/health` | Service health check |

---

## Authentication

Every API request must include a developer API key in the `Authorization` header. Keys are issued per developer account and can be scoped per environment (e.g. production, staging). Invalid or missing keys return `401 Unauthorized`. If the auth system itself is unavailable, the service returns `503 Service Unavailable` — it never fails open.

---

## File Validation Rules

Every incoming file is validated at the entry point before being queued:

| Check | Rule |
|-------|------|
| File type | Magic bytes must match JPEG, PNG, WEBP, or GIF |
| File size | Must not exceed 10MB (configurable per developer) |
| Checksum | Developer-provided checksum must match computed value |
| Rate limit | Max 100 requests per API key per minute |

---

## Key Design Decisions

**Async by default.** Uploads never block the developer's user flow. The API acknowledges immediately with a `job_id` and processes in the background.

**Developer owns criticality.** The service does not decide if an image is required. Developers choose to poll or wait for a webhook based on their own requirements.

**Retries are automatic.** The service retries failed uploads with exponential backoff up to a configurable maximum. Developers do not implement retry logic.

**Failed jobs have a TTL.** Permanently failed jobs are retained for 24 hours (configurable), giving developers a window to manually retry before the record is cleaned up.

**Idempotent uploads.** The service uses file checksums to detect duplicate uploads during retries, preventing double-storage in R2.

---

## Out of Scope

- No user interface — API only
- No image transformation (no resizing, filtering, cropping, compression)
- No CDN or image delivery/serving
- No SDK (API first; SDK is a future consideration)
- No billing or usage metering
- No direct browser-to-service uploads (developer backend is always in the middle)
- No Redis Sentinel setup (v1; high availability is a future consideration)
- No chunked account deletion (v1; standard deletion for now)

---

## Assumptions to Validate

1. **24-hour TTL is appropriate.** This was chosen as a reasonable default but may not suit all use cases. Consider making TTL configurable per upload request in a future version.

2. **10MB file size limit is sufficient.** This covers most profile pictures and document scans but may be too restrictive for high-resolution or professional use cases.

3. **Developer's server is always the upload intermediary.** This doubles bandwidth cost for the developer. Direct browser uploads via presigned URLs are a meaningful future feature.
