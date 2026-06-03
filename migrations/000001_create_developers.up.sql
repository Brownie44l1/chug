CREATE TABLE developers (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email            VARCHAR(255) UNIQUE NOT NULL,
    hashed_password  VARCHAR(255) NOT NULL,
    deleted_at       TIMESTAMP,
    created_at       TIMESTAMP NOT NULL DEFAULT now()
);
