ALTER TABLE deployments
    ADD COLUMN host_port INTEGER CHECK (host_port BETWEEN 1 AND 65535),
    ADD COLUMN start_error TEXT;
