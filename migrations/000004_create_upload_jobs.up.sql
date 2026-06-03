CREATE TYPE job_status AS ENUM (
    'pending',
    'processing',
    'success',
    'failed',
    'cancelled'
);
 
CREATE TABLE upload_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    developer_id    UUID NOT NULL REFERENCES developers(id),
    api_key_id      UUID NOT NULL REFERENCES api_keys(id),
    status          job_status NOT NULL DEFAULT 'pending',
    file_name       VARCHAR(255) NOT NULL,
    file_size_bytes INTEGER NOT NULL,
    mime_type       VARCHAR(100) NOT NULL,
    checksum        VARCHAR(255) NOT NULL,
    storage_url     VARCHAR(500),
    failure_reason  TEXT,
    retry_count     INTEGER NOT NULL DEFAULT 0,
    max_retries     INTEGER NOT NULL DEFAULT 3,
    expires_at      TIMESTAMP,
    created_at      TIMESTAMP NOT NULL DEFAULT now(),
    updated_at      TIMESTAMP NOT NULL DEFAULT now()
);
 
CREATE INDEX ON upload_jobs(developer_id);
CREATE INDEX ON upload_jobs(api_key_id);
CREATE INDEX ON upload_jobs(status, expires_at);
CREATE INDEX ON upload_jobs(checksum);
