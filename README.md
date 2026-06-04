# Chug — Image Upload & Processing Service

Chug is a highly reliable, asynchronous image upload and processing service built in Go. It offloads slow network uploads and image storage flows from developer backend servers using a queue-worker architecture, ensuring that image processing never blocks time-sensitive operations like user registrations.

---

## System Architecture

Chug consists of:
1. **API Server (Go/Gin)**: Validates incoming files (magic bytes, size, checksum) and immediately returns a `202 Accepted` response with a UUID `job_id`.
2. **Message Queue (Redis/Asynq)**: Stores and schedules upload jobs, managing retries and backoff.
3. **Background Worker (Go)**: Dequeues jobs, checks idempotency (via file checksums), uploads images to Cloudflare R2 storage, updates the database, and fires webhook notifications to registered developer endpoints.
4. **PostgreSQL Database**: Serves as the source of truth for developer configurations, API keys, upload jobs, and webhook delivery records.

```mermaid
flowchart LR
    Client((Developer App))
    Webhook((Webhook Endpoint))

    subgraph service[Chug Service]
        API[API Server]
        Queue[(Redis Queue)]
        Worker[Background Worker]
        DB[(PostgreSQL)]
        R2[(Cloudflare R2)]

        API <--> DB
        API -->|Enqueue| Queue
        Queue --> Worker
        Worker <--> DB
        Worker --> R2
    end

    Client -->|POST /uploads| API
    Client <-->|GET /uploads/:job_id| API
    Worker -->|POST Callback| Webhook
```

---

## Features

- **Fast Acknowledgment**: Responds to uploads in under 500ms by processing uploads asynchronously.
- **Strict File Validation**: Enforces type checks using file magic bytes (accepts JPEG, PNG, WEBP, GIF), size limit boundaries, and SHA256 checksum validations.
- **Idempotent Processing**: Reuses existing Cloudflare R2 storage URLs if a matching checksum has already been uploaded successfully, saving storage space and bandwidth.
- **Automatic Upload Retries**: Leverages exponential backoff (`30s → 5m → 30m`) for R2 storage network failures.
- **Webhook Delivery & Retries**: Delivers job status notifications to registered developer HTTPS endpoints with HMAC signatures (`X-Webhook-Signature`) and retries failed delivery attempts up to 4 times (`30s → 5m → 30m → 1h`).

---

## API Endpoints

### Uploads
- `POST /uploads` - Submit a new image file and checksum.
- `GET /uploads/:job_id` - Fetch current job status and storage URL.
- `POST /uploads/:job_id/retry` - Manually retry a permanently failed job within its TTL.

### Webhooks
- `POST /webhooks` - Register a callback URL (HTTPS). Returns the HMAC signing secret once.
- `GET /webhooks` - List registered webhook endpoints (excludes signing secrets).
- `DELETE /webhooks/:id` - Deactivate a webhook endpoint.
- `GET /webhooks/deliveries/:job_id` - Query webhook delivery history/logs for a specific upload job.

### System
- `GET /health` - Service health status check.

---

## Getting Started

### 1. Configuration (`.env`)
Create a `.env` file from `.env.example` and populate your database, Redis, and R2 credentials:
```env
PORT=8080
ENV=development

POSTGRES_URL=postgres://user:pass@localhost:5432/chug?sslmode=disable
REDIS_URL=redis://localhost:6379/0

DEFAULT_API_KEY=chug_demo_developer_key
API_KEY_HASH_SECRET=your_auth_hash_secret_key

QUEUE_NAME=uploads:default
WORKER_CONCURRENCY=10

R2_BUCKET_NAME=your-bucket-name
R2_ACCOUNT_ID=your-account-id
R2_ACCESS_KEY_ID=your-access-key-id
R2_SECRET_ACCESS_KEY=your-secret-access-key
R2_PUBLIC_URL=https://pub-your-id.r2.dev
```

### 2. Run Database Migrations
Make sure `golang-migrate` is installed, then run:
```bash
make migrate-up
```

### 3. Run the Services
Start the API server:
```bash
make run-api
```

Start the background queue worker:
```bash
make run-worker
```

### 4. Running Tests
Run the entire test suite:
```bash
make test
```

### 5. Running Load Tests & Benchmarks
We provide a built-in CLI load-testing tool to measure API latency, background processing speed, and idempotency efficiency under load:
```bash
make run-benchmark
```
*You can customize parameters directly via command line:*
```bash
go run cmd/benchmark/main.go -server "http://localhost:8080" -total 100 -concurrency 10 -dups 30
```
