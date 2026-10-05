CREATE TABLE workers (
    id UUID PRIMARY KEY,
    hostname TEXT NOT NULL,
    address TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('HEALTHY', 'UNHEALTHY')),
    total_cpu DOUBLE PRECISION NOT NULL CHECK (total_cpu >= 0),
    available_cpu DOUBLE PRECISION NOT NULL CHECK (available_cpu >= 0 AND available_cpu <= total_cpu),
    total_memory BIGINT NOT NULL CHECK (total_memory >= 0),
    available_memory BIGINT NOT NULL CHECK (available_memory >= 0 AND available_memory <= total_memory),
    running_containers INTEGER NOT NULL CHECK (running_containers >= 0),
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX workers_heartbeat_idx ON workers (last_heartbeat) WHERE status = 'HEALTHY';
