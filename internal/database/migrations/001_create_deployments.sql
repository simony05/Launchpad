CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TYPE deployment_status AS ENUM (
    'PENDING',
    'BUILDING',
    'STARTING',
    'RUNNING',
    'FAILED',
    'STOPPED'
);

CREATE TABLE deployments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    status deployment_status NOT NULL DEFAULT 'PENDING',
    runtime TEXT NOT NULL CHECK (runtime = 'python'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    container_id TEXT,
    internal_port INTEGER CHECK (internal_port BETWEEN 1 AND 65535),
    public_identifier TEXT UNIQUE
);

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deployments_set_updated_at
BEFORE UPDATE ON deployments
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX deployments_created_at_idx ON deployments (created_at DESC);
