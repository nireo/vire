CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS account_members (
    account_id TEXT NOT NULL REFERENCES accounts(id),
    user_id TEXT NOT NULL REFERENCES users(id),
    role TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    PRIMARY KEY (account_id, user_id)
);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id),
    created_by_user_id TEXT NOT NULL REFERENCES users(id),
    name TEXT NOT NULL,
    token_hash BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS api_keys_account_idx ON api_keys(account_id);

CREATE TABLE IF NOT EXISTS model_prices (
    id TEXT PRIMARY KEY,
    model TEXT NOT NULL,
    input_rate_micro_per_million BIGINT NOT NULL CHECK (input_rate_micro_per_million >= 0),
    output_rate_micro_per_million BIGINT NOT NULL CHECK (output_rate_micro_per_million >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS model_prices_model_latest_idx ON model_prices(model, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS usage_events (
    request_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id),
    key_id TEXT NOT NULL REFERENCES api_keys(id),
    model TEXT NOT NULL,
    backend TEXT NOT NULL DEFAULT '',
    price_id TEXT REFERENCES model_prices(id),
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'complete', 'incomplete', 'failed')),
    http_status INTEGER,
    prompt_tokens BIGINT CHECK (prompt_tokens >= 0),
    completion_tokens BIGINT CHECK (completion_tokens >= 0),
    estimated_cost_micro BIGINT CHECK (estimated_cost_micro >= 0),
    rolled_up_at TIMESTAMPTZ,
    CHECK (state <> 'complete' OR (prompt_tokens IS NOT NULL AND completion_tokens IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS usage_events_account_time_idx ON usage_events(account_id, started_at DESC);
CREATE INDEX IF NOT EXISTS usage_events_unrolled_idx ON usage_events(started_at) WHERE rolled_up_at IS NULL AND state <> 'pending';
CREATE INDEX IF NOT EXISTS usage_events_retention_idx ON usage_events(started_at) WHERE rolled_up_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS usage_daily (
    day DATE NOT NULL,
    account_id TEXT NOT NULL REFERENCES accounts(id),
    key_id TEXT NOT NULL REFERENCES api_keys(id),
    model TEXT NOT NULL,
    request_count BIGINT NOT NULL DEFAULT 0,
    complete_count BIGINT NOT NULL DEFAULT 0,
    incomplete_count BIGINT NOT NULL DEFAULT 0,
    failed_count BIGINT NOT NULL DEFAULT 0,
    prompt_tokens BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    estimated_cost_micro BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, account_id, key_id, model)
);
