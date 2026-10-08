ALTER TYPE deployment_status ADD VALUE IF NOT EXISTS 'EXPIRING';
ALTER TYPE deployment_status ADD VALUE IF NOT EXISTS 'EXPIRED';

ALTER TABLE deployments
    ADD COLUMN ttl_seconds BIGINT NOT NULL DEFAULT 0 CHECK (ttl_seconds >= 0),
    ADD COLUMN expires_at TIMESTAMPTZ,
    ADD COLUMN expiration_attempts INTEGER NOT NULL DEFAULT 0 CHECK (expiration_attempts >= 0),
    ADD COLUMN expiration_retry_at TIMESTAMPTZ,
    ADD COLUMN expiration_error TEXT,
    ADD CONSTRAINT deployments_ttl_expiry_consistent CHECK ((ttl_seconds=0 AND expires_at IS NULL) OR (ttl_seconds>0 AND expires_at IS NOT NULL));

CREATE INDEX deployments_expiration_idx
    ON deployments (expires_at)
    WHERE expires_at IS NOT NULL;
