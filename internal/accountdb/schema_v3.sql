CREATE TABLE user_passwords (
    user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    password_hash TEXT NOT NULL
);
CREATE TABLE portal_sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX portal_sessions_expiry_idx ON portal_sessions(expires_at);
CREATE TABLE login_attempts (
    email TEXT PRIMARY KEY,
    attempts INTEGER NOT NULL,
    window_started TIMESTAMPTZ NOT NULL
);
