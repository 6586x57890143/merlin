CREATE TABLE scheduler_job_state (
    job_key              TEXT PRIMARY KEY,
    last_run             TIMESTAMPTZ,
    consecutive_failures INT NOT NULL DEFAULT 0,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
