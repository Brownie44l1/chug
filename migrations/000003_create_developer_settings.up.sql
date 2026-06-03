CREATE TABLE developer_settings (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    developer_id         UUID UNIQUE NOT NULL REFERENCES developers(id),
    max_file_size_bytes  INTEGER NOT NULL DEFAULT 10485760,
    max_retries          INTEGER NOT NULL DEFAULT 3,
    ttl_hours            INTEGER NOT NULL DEFAULT 24,
    monthly_upload_limit INTEGER NOT NULL DEFAULT 100,
    created_at           TIMESTAMP NOT NULL DEFAULT now(),
    updated_at           TIMESTAMP NOT NULL DEFAULT now()
);
