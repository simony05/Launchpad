ALTER TABLE deployments
 ADD COLUMN build_completed_at TIMESTAMPTZ,
 ADD COLUMN startup_failed_at TIMESTAMPTZ;
