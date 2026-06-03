CREATE TABLE api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    developer_id UUID NOT NULL REFERENCES developers(id),
    key_hash     VARCHAR(255) UNIQUE NOT NULL,
    label        VARCHAR(100) NOT NULL,
    is_active    BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMP NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMP
);
 
CREATE INDEX ON api_keys(developer_id);
CREATE INDEX ON api_keys(key_hash);
