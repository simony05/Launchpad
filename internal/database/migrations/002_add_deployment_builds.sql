ALTER TYPE deployment_status ADD VALUE IF NOT EXISTS 'READY_TO_START';

ALTER TABLE deployments
    ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    ADD COLUMN image_name TEXT,
    ADD COLUMN build_log TEXT,
    ADD COLUMN build_error TEXT;
