-- Webhook subscriptions for a set of contracts. The id doubles as the secret
-- that signs deliveries; contract_ids is a comma-separated list.
CREATE TABLE subscriptions (
    id           TEXT    PRIMARY KEY,
    network      TEXT    NOT NULL,
    contract_ids TEXT    NOT NULL,
    url          TEXT    NOT NULL,
    active       INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
