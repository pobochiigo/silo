CREATE TABLE IF NOT EXISTS accounts (
    id      TEXT PRIMARY KEY,
    owner   TEXT NOT NULL,
    balance BIGINT NOT NULL CHECK (balance >= 0)
);

CREATE TABLE IF NOT EXISTS entries (
    id         BIGSERIAL PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts (id),
    amount     BIGINT NOT NULL,
    memo       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
