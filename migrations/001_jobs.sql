CREATE TABLE IF NOT EXISTS image_users (
    username text PRIMARY KEY,
    password_hash text NOT NULL
);

CREATE TABLE IF NOT EXISTS image_jobs (
    id uuid PRIMARY KEY,
    owner text NOT NULL REFERENCES image_users(username),
    filter text NOT NULL,
    source_key text NOT NULL,
    request_hash text NOT NULL,
    idempotency_key text,
    status text NOT NULL CHECK (status IN ('queued','processing','succeeded','failed')),
    attempts integer NOT NULL DEFAULT 0,
    lease_token uuid,
    lease_until timestamptz,
    result_key text,
    error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner, idempotency_key)
);

CREATE INDEX IF NOT EXISTS image_jobs_pending ON image_jobs (status, created_at)
    WHERE status IN ('queued','processing');
CREATE INDEX IF NOT EXISTS image_jobs_leases ON image_jobs (lease_until) WHERE status = 'processing';

CREATE TABLE IF NOT EXISTS image_outbox (
    job_id uuid PRIMARY KEY REFERENCES image_jobs(id) ON DELETE CASCADE,
    available_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

CREATE INDEX IF NOT EXISTS image_outbox_pending ON image_outbox (available_at)
    WHERE published_at IS NULL;
