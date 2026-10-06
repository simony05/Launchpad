ALTER TYPE deployment_status ADD VALUE IF NOT EXISTS 'RECOVERING';
ALTER TABLE workers
 ADD COLUMN instance_id TEXT NOT NULL DEFAULT '',
 ADD COLUMN recovery_state TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (recovery_state IN ('ACTIVE','FENCING','FENCED')),
 ADD COLUMN fenced_at TIMESTAMPTZ,
 ADD COLUMN fencing_attempted_at TIMESTAMPTZ,
 ADD COLUMN fencing_error TEXT;
CREATE UNIQUE INDEX workers_instance_identity ON workers(instance_id) WHERE instance_id <> '';
ALTER TABLE deployments
 ADD COLUMN source_files JSONB,
 ADD COLUMN failover_attempts INTEGER NOT NULL DEFAULT 0,
 ADD COLUMN recovery_error TEXT,
 ADD COLUMN recovery_started_at TIMESTAMPTZ;
ALTER TABLE deployments ADD COLUMN recovery_retries INTEGER NOT NULL DEFAULT 0;
