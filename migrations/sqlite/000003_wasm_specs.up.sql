-- Uploaded code can be catalogued before any contract instance uses it.
CREATE TABLE wasm_specs (
    network    TEXT NOT NULL,
    wasm_hash  TEXT NOT NULL,
    spec       TEXT NOT NULL,
    decoded_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    PRIMARY KEY (network, wasm_hash)
);
