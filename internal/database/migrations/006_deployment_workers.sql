ALTER TABLE deployments
    ADD COLUMN worker_id UUID REFERENCES workers(id),
    ADD COLUMN worker_address TEXT,
    ADD COLUMN reserved_cpu DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (reserved_cpu >= 0),
    ADD COLUMN reserved_memory BIGINT NOT NULL DEFAULT 0 CHECK (reserved_memory >= 0),
    ADD COLUMN capacity_reserved BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX deployments_worker_reservations ON deployments(worker_id) WHERE capacity_reserved;
