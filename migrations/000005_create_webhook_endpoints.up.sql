CREATE TABLE webhook_endpoints (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    developer_id UUID NOT NULL REFERENCES developers(id),
    url          VARCHAR(500) NOT NULL,
    secret       VARCHAR(255) NOT NULL,
    is_active    BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMP NOT NULL DEFAULT now()
);
 
CREATE INDEX ON webhook_endpoints(developer_id);
