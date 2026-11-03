-- Registered contracts, namespaced by network so the same contract ID on
-- testnet and mainnet stay separate records.
CREATE TABLE contracts (
    network            TEXT NOT NULL,
    contract_id        TEXT NOT NULL,
    current_wasm_hash  TEXT NOT NULL,
    name               TEXT NOT NULL DEFAULT '',
    first_seen         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_refreshed     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    PRIMARY KEY (network, contract_id)
);

-- One immutable row per interface a contract has ever run. Refreshing an
-- upgraded contract appends here rather than overwriting, so superseded
-- interfaces stay retrievable.
CREATE TABLE specs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    network     TEXT    NOT NULL,
    contract_id TEXT    NOT NULL,
    wasm_hash   TEXT    NOT NULL,
    spec        TEXT    NOT NULL,
    decoded_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    UNIQUE (network, contract_id, wasm_hash),
    FOREIGN KEY (network, contract_id) REFERENCES contracts (network, contract_id) ON DELETE CASCADE
);

-- Version history is "every row for one contract, newest first".
CREATE INDEX specs_by_contract ON specs (network, contract_id, decoded_at DESC);

-- The default listing is "most recently refreshed first within a network".
CREATE INDEX contracts_by_last_refreshed ON contracts (network, last_refreshed DESC);
