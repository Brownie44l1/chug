# Entity Relationship Document (ERD)
## Image Upload & Processing Service

---

## Entities

### developers
The root account entity. Everything else belongs to a developer.

```sql
developers
─────────────────────────────────────────
id                UUID          PRIMARY KEY
email             VARCHAR       UNIQUE NOT NULL
hashed_password   VARCHAR       NOT NULL
deleted_at        TIMESTAMP     NULLABLE          -- soft delete
created_at        TIMESTAMP     NOT NULL
```

> `deleted_at` enables soft deletion. When a developer requests account deletion, this field is set and all API keys are immediately revoked. Hard deletion and R2 file cleanup are scheduled as a background job after a retention period.

---

### api_keys
Multiple keys per developer. Each key can be independently labelled, activated, and revoked.

```sql
api_keys
─────────────────────────────────────────
id                UUID          PRIMARY KEY
developer_id      UUID          FK → developers.id
key_hash          VARCHAR       UNIQUE NOT NULL    -- never store raw key
label             VARCHAR       NOT NULL           -- e.g. "production", "staging"
is_active         BOOLEAN       NOT NULL DEFAULT true
created_at        TIMESTAMP     NOT NULL
revoked_at        TIMESTAMP     NULLABLE
```

> The raw API key is shown to the developer exactly once at creation. Only the hash is stored. On each request, the incoming key is hashed and compared — the same principle as password hashing.

> Multiple keys per developer allow environment isolation. A compromised staging key can be revoked without touching production.

---

### developer_settings
One-to-one with developers. Stores configurable limits and defaults per account.

```sql
developer_settings
─────────────────────────────────────────
id                    UUID      PRIMARY KEY
developer_id          UUID      UNIQUE FK → developers.id
max_file_size_bytes   INTEGER   NOT NULL DEFAULT 10485760   -- 10MB
max_retries           INTEGER   NOT NULL DEFAULT 3
ttl_hours             INTEGER   NOT NULL DEFAULT 24
monthly_upload_limit  INTEGER   NOT NULL DEFAULT 100
created_at            TIMESTAMP NOT NULL
updated_at            TIMESTAMP NOT NULL
```

---

### upload_jobs
The core entity. Every upload attempt creates one job record.

```sql
upload_jobs
─────────────────────────────────────────
id                UUID          PRIMARY KEY
developer_id      UUID          FK → developers.id
api_key_id        UUID          FK → api_keys.id
status            ENUM          NOT NULL DEFAULT 'pending'
                                -- pending | processing | success
                                -- failed | cancelled
file_name         VARCHAR       NOT NULL
file_size_bytes   INTEGER       NOT NULL
mime_type         VARCHAR       NOT NULL
checksum          VARCHAR       NOT NULL     -- idempotency guard
storage_url       VARCHAR       NULLABLE     -- populated on success
failure_reason    TEXT          NULLABLE     -- populated on failure
retry_count       INTEGER       NOT NULL DEFAULT 0
max_retries       INTEGER       NOT NULL DEFAULT 3
expires_at        TIMESTAMP     NULLABLE     -- TTL for failed jobs
created_at        TIMESTAMP     NOT NULL
updated_at        TIMESTAMP     NOT NULL
```

> `developer_id` and `api_key_id` are both stored intentionally. `developer_id` identifies ownership. `api_key_id` enables per-key audit trails — e.g. "show all uploads made with my production key this month."

> `checksum` enables idempotent retries. Before uploading to R2, the worker checks if a file with this checksum already exists. If it does, the upload is skipped and the existing URL is used.

> `cancelled` status is set on all pending and processing jobs when a developer deletes their account, preventing workers from processing jobs for a deleted account.

---

### webhook_endpoints
Where to send notifications for a developer's jobs.

```sql
webhook_endpoints
─────────────────────────────────────────
id                UUID          PRIMARY KEY
developer_id      UUID          FK → developers.id
url               VARCHAR       NOT NULL
secret            VARCHAR       NOT NULL    -- HMAC signing secret
is_active         BOOLEAN       NOT NULL DEFAULT true
created_at        TIMESTAMP     NOT NULL
```

> The `secret` is used to sign every webhook payload with HMAC. Developers verify the signature on receipt to confirm the webhook genuinely came from this service and not an attacker. The secret is shown once at registration and never returned again.

---

### webhook_deliveries
An audit log of every webhook delivery attempt.

```sql
webhook_deliveries
─────────────────────────────────────────
id                    UUID      PRIMARY KEY
upload_job_id         UUID      FK → upload_jobs.id
webhook_endpoint_id   UUID      FK → webhook_endpoints.id
status                ENUM      NOT NULL DEFAULT 'pending'
                                -- pending | delivered | failed
attempt_count         INTEGER   NOT NULL DEFAULT 0
last_attempted_at     TIMESTAMP NULLABLE
delivered_at          TIMESTAMP NULLABLE
response_status       INTEGER   NULLABLE    -- HTTP status returned by developer
created_at            TIMESTAMP NOT NULL
```

> Without this table, there is no way to know if a developer actually received a webhook or if delivery failed silently. This table answers "did they get it?" and enables the `GET /webhooks/deliveries/:job_id` endpoint.

---

## Relationships

```
developers ──────────────< api_keys
    │                         │
    │                         │
    ├──────────────1 developer_settings
    │
    ├──────────────< upload_jobs
    │                    │
    │                    └──────────< webhook_deliveries
    │                                        │
    └──────────────< webhook_endpoints >─────┘
```

| Relationship | Type | Description |
|---|---|---|
| developer → api_keys | one-to-many | A developer has many keys |
| developer → developer_settings | one-to-one | One settings record per developer |
| developer → upload_jobs | one-to-many | A developer submits many jobs |
| developer → webhook_endpoints | one-to-many | A developer registers many endpoints |
| api_key → upload_jobs | one-to-many | Each job is tied to the key that created it |
| upload_job → webhook_deliveries | one-to-many | Each job can have multiple delivery attempts |
| webhook_endpoint → webhook_deliveries | one-to-many | Each endpoint tracks its own deliveries |

---

## Recommended Indexes

```sql
-- Auth: fast API key lookup
CREATE INDEX ON api_keys(key_hash);
CREATE INDEX ON api_keys(developer_id);

-- Job queries by developer
CREATE INDEX ON upload_jobs(developer_id);
CREATE INDEX ON upload_jobs(api_key_id);

-- TTL cleanup cron: find expired failed jobs
CREATE INDEX ON upload_jobs(status, expires_at);

-- Idempotency: detect duplicate uploads
CREATE INDEX ON upload_jobs(checksum);

-- Webhook delivery lookups
CREATE INDEX ON webhook_deliveries(upload_job_id);
```

---

## Account Deletion Behaviour

When a developer requests account deletion:

```
1. Set developers.deleted_at = now()           -- soft delete
2. Set all api_keys.is_active = false          -- revoke all keys immediately
3. Set all pending/processing jobs to cancelled -- stop in-flight work
4. Schedule background deletion job:
   └── Chunk through upload_jobs for this developer
       └── For each job with a storage_url:
           └── Delete file from R2
           └── Checkpoint progress
   └── Delete webhook_deliveries records
   └── Delete webhook_endpoints records
   └── Delete upload_jobs records
   └── Delete api_keys records
   └── Delete developer_settings record
   └── Delete developer record
```

> R2 files are deleted before database records. If the job crashes mid-way, the storage URLs are still in the database and the cleanup job can resume from the last checkpoint. Deleting database records first would make R2 cleanup impossible to recover.

> Chunked deletion prevents orphaned files from accumulating if the cleanup job crashes partway through a large account.
