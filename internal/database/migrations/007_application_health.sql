ALTER TABLE deployments
 ADD COLUMN health_path TEXT NOT NULL DEFAULT '',
 ADD COLUMN health_state TEXT CHECK (health_state IN ('RUNNING','UNHEALTHY','EXITED')),
 ADD COLUMN health_checked_at TIMESTAMPTZ,
 ADD COLUMN restart_attempts INTEGER NOT NULL DEFAULT 0 CHECK (restart_attempts >= 0),
 ADD COLUMN next_restart_at TIMESTAMPTZ,
 ADD COLUMN last_exit_code INTEGER,
 ADD COLUMN oom_killed BOOLEAN NOT NULL DEFAULT FALSE,
 ADD COLUMN runtime_error TEXT,
 ADD COLUMN last_failure TEXT,
 ADD COLUMN runtime_logs TEXT NOT NULL DEFAULT '';
