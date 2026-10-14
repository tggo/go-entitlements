CREATE TABLE IF NOT EXISTS entitlements (
    source         text        NOT NULL,
    original_tx_id text        NOT NULL,
    user_id        text        NOT NULL,
    product_id     text        NOT NULL,
    state          text        NOT NULL,
    expires_at     timestamptz,
    updated_at     timestamptz NOT NULL,
    version        bigint      NOT NULL,
    PRIMARY KEY (source, original_tx_id)
);

CREATE INDEX IF NOT EXISTS entitlements_user_idx ON entitlements (user_id);

CREATE TABLE IF NOT EXISTS processed_events (
    event_id       text        PRIMARY KEY,
    source         text        NOT NULL,
    original_tx_id text        NOT NULL,
    raw            bytea,
    applied        boolean     NOT NULL,
    reason         text        NOT NULL DEFAULT '',
    occurred_at    timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
