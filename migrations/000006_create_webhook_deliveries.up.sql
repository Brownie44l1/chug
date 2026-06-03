CREATE TYPE delivery_status AS ENUM (
    'pending',
    'delivered',
    'failed'
);
 
CREATE TABLE webhook_deliveries (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    upload_job_id       UUID NOT NULL REFERENCES upload_jobs(id),
    webhook_endpoint_id UUID NOT NULL REFERENCES webhook_endpoints(id),
    status              delivery_status NOT NULL DEFAULT 'pending',
    attempt_count       INTEGER NOT NULL DEFAULT 0,
    last_attempted_at   TIMESTAMP,
    delivered_at        TIMESTAMP,
    response_status     INTEGER,
    created_at          TIMESTAMP NOT NULL DEFAULT now()
);
 
CREATE INDEX ON webhook_deliveries(upload_job_id);
