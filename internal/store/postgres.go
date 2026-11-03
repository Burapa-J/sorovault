package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/spec"
)

// Postgres is the Store backed by a Postgres database.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// PoolOptions sizes the Postgres connection pool.
//
// A zero field leaves pgx's own default in place, so a caller that does not
// care about the size gets the pool it would get by handing the connection
// string to pgx unchanged.
type PoolOptions struct {
	// MaxConns is the maximum number of connections the pool holds open.
	MaxConns int32
	// MinConns is the number of connections the pool keeps open between
	// requests.
	MinConns int32
}

// NewPostgres connects to databaseURL and verifies the connection.
func NewPostgres(ctx context.Context, databaseURL string, opts PoolOptions) (*Postgres, error) {
	poolCfg, err := ResolvePoolConfig(databaseURL, opts)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: pinging postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// ResolvePoolConfig parses databaseURL and applies opts to it, returning the
// pool configuration a store would use.
//
// An explicit size wins over the pool_max_conns and pool_min_conns
// parameters in the connection string. The size is this deployment's own
// knob, while a connection string is often shared with other tools that
// have no business being moved by this deployment's settings.
//
// It is exported so that the precedence can be checked without a database.
func ResolvePoolConfig(databaseURL string, opts PoolOptions) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: parsing postgres connection string: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	// pgx does not check this, and a pool asked for more connections than
	// its maximum would wait for one that can never be free.
	if cfg.MinConns > cfg.MaxConns {
		return nil, fmt.Errorf("store: pool minimum of %d connections exceeds its maximum of %d",
			cfg.MinConns, cfg.MaxConns)
	}
	return cfg, nil
}

// PoolConfig reports the pool settings in force: the connection string's
// parameters with any configured override applied on top.
func (p *Postgres) PoolConfig() PoolOptions {
	cfg := p.pool.Config()
	return PoolOptions{MaxConns: cfg.MaxConns, MinConns: cfg.MinConns}
}

// NewPostgresFromPool wraps an existing pool. Tests use it to share a pool
// across cases; Close then leaves the pool open for the caller to manage.
func NewPostgresFromPool(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// Ping checks that the database at databaseURL answers.
//
// It opens nothing but a connection and changes nothing, so a health check
// can use it without leaving a trace.
func Ping(ctx context.Context, databaseURL string) error {
	if IsSQLiteURL(databaseURL) {
		return pingSQLite(ctx, databaseURL)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("store: connecting to postgres: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: pinging postgres: %w", err)
	}
	return nil
}

// Ping reports whether the database still answers, without changing it.
func (p *Postgres) Ping(ctx context.Context) error {
	if err := p.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: pinging postgres: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

// SaveWasmSpec stores an interface for uploaded code independent of contracts.
func (p *Postgres) SaveWasmSpec(ctx context.Context, network, wasmHash string, iface *model.Interface) error {
	if iface == nil {
		return errors.New("store: cannot save a nil interface")
	}
	body, err := json.Marshal(iface)
	if err != nil {
		return fmt.Errorf("store: encoding interface: %w", err)
	}
	const q = `
		INSERT INTO wasm_specs (network, wasm_hash, spec)
		VALUES ($1, $2, $3)
		ON CONFLICT (network, wasm_hash) DO UPDATE SET spec = EXCLUDED.spec, decoded_at = now()`
	if _, err := p.pool.Exec(ctx, q, network, wasmHash, body); err != nil {
		return fmt.Errorf("store: saving wasm spec %s: %w", wasmHash, err)
	}
	return nil
}

// GetWasmSpec returns a hash-addressed interface without requiring a contract.
func (p *Postgres) GetWasmSpec(ctx context.Context, network, wasmHash string) (*WasmSpec, error) {
	const q = `SELECT network, wasm_hash, spec, decoded_at FROM wasm_specs WHERE network = $1 AND wasm_hash = $2`
	var s WasmSpec
	var body []byte
	err := p.pool.QueryRow(ctx, q, network, wasmHash).Scan(&s.Network, &s.WasmHash, &body, &s.DecodedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: wasm spec %s on %s", ErrNotFound, wasmHash, network)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading wasm spec %s: %w", wasmHash, err)
	}
	var iface model.Interface
	if err := json.Unmarshal(body, &iface); err != nil {
		return nil, fmt.Errorf("store: decoding wasm spec %s: %w", wasmHash, err)
	}
	s.Interface = &iface
	return &s, nil
}

// Save upserts the contract and appends its spec version, in one transaction
// so a contract row can never end up pointing at a spec that was not stored.
func (p *Postgres) Save(ctx context.Context, c Contract, iface *model.Interface) error {
	if iface == nil {
		return errors.New("store: cannot save a nil interface")
	}

	body, err := json.Marshal(iface)
	if err != nil {
		return fmt.Errorf("store: encoding interface: %w", err)
	}
	c.Implements = spec.ImplementedStandards(iface)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once the tx is committed

	// first_seen is preserved across refreshes; everything else tracks the
	// most recent successful fetch. Saving an interface means the contract
	// was live, so a missing mark is cleared.
	const upsertContract = `
		INSERT INTO contracts (network, contract_id, current_wasm_hash, name, implements, first_seen, last_refreshed)
		VALUES ($1, $2, $3, $4, $5, now(), now())
		ON CONFLICT (network, contract_id) DO UPDATE SET
			current_wasm_hash = EXCLUDED.current_wasm_hash,
			name              = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE contracts.name END,
			implements        = EXCLUDED.implements,
			last_refreshed    = now(),
			missing_since     = NULL`

	if _, err := tx.Exec(ctx, upsertContract, c.Network, c.ContractID, c.CurrentWasmHash, c.Name, c.Implements); err != nil {
		return fmt.Errorf("store: saving contract %s: %w", c.ContractID, err)
	}

	// Re-registering an unchanged contract must not append a duplicate
	// version, so a repeat hash refreshes the existing row in place.
	const insertSpec = `
		INSERT INTO specs (network, contract_id, wasm_hash, spec, symbols, docs_tsv, observed_ledger)
		VALUES ($1, $2, $3, $4, $5, to_tsvector('simple', $6), $7)
		ON CONFLICT (network, contract_id, wasm_hash) DO UPDATE SET
			spec     = EXCLUDED.spec,
			symbols  = EXCLUDED.symbols,
			docs_tsv = EXCLUDED.docs_tsv`

	if _, err := tx.Exec(ctx, insertSpec,
		c.Network, c.ContractID, c.CurrentWasmHash, body, joinSymbols(iface), joinDocs(iface), c.ObservedLedger); err != nil {
		return fmt.Errorf("store: saving spec for %s: %w", c.ContractID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// GetContract returns one contract's current state.
func (p *Postgres) GetContract(ctx context.Context, network, contractID string) (*Contract, error) {
	const q = `
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit, first_seen, last_refreshed, missing_since,
		       implements, deprecated_since, successor, deprecation_note, creator, created_ledger, verified_by, verified_at, verified_wasm_hash
		FROM contracts
		WHERE network = $1 AND contract_id = $2`

	var c Contract
	err := p.pool.QueryRow(ctx, q, network, contractID).Scan(
		&c.Network, &c.ContractID, &c.CurrentWasmHash, &c.Name, &c.SourceURL, &c.SourceCommit,
		&c.FirstSeen, &c.LastRefreshed, &c.MissingSince,
		&c.Implements, &c.DeprecatedSince, &c.Successor, &c.DeprecationNote,
		&c.Creator, &c.CreatedLedger, &c.VerifiedBy, &c.VerifiedAt, &c.VerifiedWasmHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading contract %s: %w", contractID, err)
	}
	return &c, nil
}

// MarkMissing records that a contract could not be found on the network.
//
// The first sighting is the one that counts: a contract that stays missing
// keeps the timestamp of when it went away rather than the time it was last
// looked for.
func (p *Postgres) MarkMissing(ctx context.Context, network, contractID string, since time.Time) error {
	const q = `
		UPDATE contracts SET missing_since = COALESCE(missing_since, $3)
		WHERE network = $1 AND contract_id = $2`

	tag, err := p.pool.Exec(ctx, q, network, contractID, since.UTC())
	if err != nil {
		return fmt.Errorf("store: marking contract %s missing: %w", contractID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}

// SetDeprecation marks an existing contract deprecated, with an optional
// successor contract ID and note. Verifying a successor — registered on
// the same network, and a different contract — is the caller's job.
func (p *Postgres) SetDeprecation(ctx context.Context, network, contractID, successor, note string) error {
	const q = `
		UPDATE contracts
		SET deprecated_since = now(), successor = $3, deprecation_note = $4
		WHERE network = $1 AND contract_id = $2`
	tag, err := p.pool.Exec(ctx, q, network, contractID, successor, note)
	if err != nil {
		return fmt.Errorf("store: deprecating contract %s: %w", contractID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}

// ClearDeprecation lifts the mark, restoring the contract to active use.
func (p *Postgres) ClearDeprecation(ctx context.Context, network, contractID string) error {
	const q = `
		UPDATE contracts
		SET deprecated_since = NULL, successor = '', deprecation_note = ''
		WHERE network = $1 AND contract_id = $2`
	tag, err := p.pool.Exec(ctx, q, network, contractID)
	if err != nil {
		return fmt.Errorf("store: clearing deprecation for %s: %w", contractID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}

// UpdateSource attaches repository metadata to an existing contract.
func (p *Postgres) UpdateSource(ctx context.Context, network, contractID, sourceURL, sourceCommit string) error {
	const q = `UPDATE contracts SET source_url = $3, source_commit = $4 WHERE network = $1 AND contract_id = $2`
	tag, err := p.pool.Exec(ctx, q, network, contractID, sourceURL, sourceCommit)
	if err != nil {
		return fmt.Errorf("store: updating source for %s: %w", contractID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}

// SetOrigin records who created a contract and in which ledger.
func (p *Postgres) SetOrigin(ctx context.Context, network, contractID, creator string, ledger uint32) error {
	const q = `UPDATE contracts SET creator = $3, created_ledger = $4 WHERE network = $1 AND contract_id = $2`
	tag, err := p.pool.Exec(ctx, q, network, contractID, creator, int64(ledger))
	if err != nil {
		return fmt.Errorf("store: recording origin of %s: %w", contractID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}

// SetVerified records an accepted publisher claim and the name and source it
// carried.
func (p *Postgres) SetVerified(ctx context.Context, network, contractID, name, sourceURL, sourceCommit string, v Verification) error {
	const q = `
		UPDATE contracts SET
			name = $3, source_url = $4, source_commit = $5,
			verified_by = $6, verified_at = $7, verified_wasm_hash = $8
		WHERE network = $1 AND contract_id = $2`
	tag, err := p.pool.Exec(ctx, q, network, contractID, name, sourceURL, sourceCommit,
		v.Publisher, v.VerifiedAt.UTC(), v.WasmHash)
	if err != nil {
		return fmt.Errorf("store: verifying %s: %w", contractID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}

// GetState returns a persisted value.
func (p *Postgres) GetState(ctx context.Context, key string) (string, error) {
	var value string
	err := p.pool.QueryRow(ctx, `SELECT value FROM state WHERE key = $1`, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: state %q", ErrNotFound, key)
	}
	if err != nil {
		return "", fmt.Errorf("store: reading state %q: %w", key, err)
	}
	return value, nil
}

// SetState stores a value under key.
func (p *Postgres) SetState(ctx context.Context, key, value string) error {
	const q = `
		INSERT INTO state (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`
	if _, err := p.pool.Exec(ctx, q, key, value); err != nil {
		return fmt.Errorf("store: writing state %q: %w", key, err)
	}
	return nil
}

// Delete removes a contract and every version stored for it. The specs
// foreign key cascades, so one statement removes the whole history.
func (p *Postgres) Delete(ctx context.Context, network, contractID string) error {
	const q = `DELETE FROM contracts WHERE network = $1 AND contract_id = $2`
	tag, err := p.pool.Exec(ctx, q, network, contractID)
	if err != nil {
		return fmt.Errorf("store: deleting contract %s: %w", contractID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}

// Export returns every contract and interface version in a stable order.
func (p *Postgres) Export(ctx context.Context) (*Snapshot, error) {
	const contractsQuery = `
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit,
		       first_seen, last_refreshed, missing_since,
		       implements, deprecated_since, successor, deprecation_note,
		       creator, created_ledger, verified_by, verified_at, verified_wasm_hash
		FROM contracts
		ORDER BY network, contract_id`

	rows, err := p.pool.Query(ctx, contractsQuery)
	if err != nil {
		return nil, fmt.Errorf("store: exporting contracts: %w", err)
	}
	defer rows.Close()

	snapshot := &Snapshot{Version: SnapshotVersion, Contracts: []ContractSnapshot{}}
	index := make(map[string]int)
	for rows.Next() {
		var c Contract
		if err := rows.Scan(
			&c.Network, &c.ContractID, &c.CurrentWasmHash, &c.Name, &c.SourceURL, &c.SourceCommit,
			&c.FirstSeen, &c.LastRefreshed, &c.MissingSince,
			&c.Implements, &c.DeprecatedSince, &c.Successor, &c.DeprecationNote,
			&c.Creator, &c.CreatedLedger, &c.VerifiedBy, &c.VerifiedAt, &c.VerifiedWasmHash,
		); err != nil {
			return nil, fmt.Errorf("store: scanning exported contract: %w", err)
		}
		index[key(c.Network, c.ContractID)] = len(snapshot.Contracts)
		snapshot.Contracts = append(snapshot.Contracts, ContractSnapshot{
			Contract: c,
			Versions: []SpecSnapshot{},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: exporting contracts: %w", err)
	}

	const specsQuery = `
		SELECT network, contract_id, wasm_hash, spec, decoded_at
		FROM specs
		ORDER BY network, contract_id, decoded_at, id`
	specRows, err := p.pool.Query(ctx, specsQuery)
	if err != nil {
		return nil, fmt.Errorf("store: exporting specs: %w", err)
	}
	defer specRows.Close()

	for specRows.Next() {
		var network, contractID, wasmHash string
		var body []byte
		var decodedAt time.Time
		if err := specRows.Scan(&network, &contractID, &wasmHash, &body, &decodedAt); err != nil {
			return nil, fmt.Errorf("store: scanning exported spec: %w", err)
		}
		var iface model.Interface
		if err := json.Unmarshal(body, &iface); err != nil {
			return nil, fmt.Errorf("store: decoding exported spec for %s: %w", contractID, err)
		}
		i, ok := index[key(network, contractID)]
		if !ok {
			continue
		}
		snapshot.Contracts[i].Versions = append(snapshot.Contracts[i].Versions, SpecSnapshot{
			WasmHash:  wasmHash,
			Interface: &iface,
			DecodedAt: decodedAt,
		})
	}
	if err := specRows.Err(); err != nil {
		return nil, fmt.Errorf("store: exporting specs: %w", err)
	}
	return snapshot, nil
}

// Import loads a snapshot in one transaction, preserving every timestamp.
func (p *Postgres) Import(ctx context.Context, snapshot *Snapshot, merge bool) error {
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin import: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once the tx is committed

	if !merge {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contracts)`).Scan(&exists); err != nil {
			return fmt.Errorf("store: checking registry is empty: %w", err)
		}
		if exists {
			return ErrRegistryNotEmpty
		}
	}

	const insertContract = `
		INSERT INTO contracts (
			network, contract_id, current_wasm_hash, name, source_url, source_commit,
			first_seen, last_refreshed, missing_since,
			implements, deprecated_since, successor, deprecation_note,
			creator, created_ledger, verified_by, verified_at, verified_wasm_hash
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (network, contract_id) DO NOTHING`

	const insertSpec = `
		INSERT INTO specs (network, contract_id, wasm_hash, spec, symbols, docs_tsv, decoded_at)
		VALUES ($1, $2, $3, $4, $5, to_tsvector('simple', $6), $7)
		ON CONFLICT (network, contract_id, wasm_hash) DO NOTHING`

	for _, cs := range snapshot.Contracts {
		c := cs.Contract
		if _, err := tx.Exec(ctx, insertContract,
			c.Network, c.ContractID, c.CurrentWasmHash, c.Name, c.SourceURL, c.SourceCommit,
			c.FirstSeen, c.LastRefreshed, c.MissingSince,
			c.Implements, c.DeprecatedSince, c.Successor, c.DeprecationNote,
			c.Creator, int64(c.CreatedLedger), c.VerifiedBy, c.VerifiedAt, c.VerifiedWasmHash); err != nil {
			return fmt.Errorf("store: importing contract %s: %w", c.ContractID, err)
		}

		for _, version := range cs.Versions {
			body, err := json.Marshal(version.Interface)
			if err != nil {
				return fmt.Errorf("store: encoding imported spec for %s: %w", c.ContractID, err)
			}
			if _, err := tx.Exec(ctx, insertSpec,
				c.Network, c.ContractID, version.WasmHash, body,
				joinSymbols(version.Interface), joinDocs(version.Interface), version.DecodedAt); err != nil {
				return fmt.Errorf("store: importing spec for %s: %w", c.ContractID, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit import: %w", err)
	}
	return nil
}

// ListContracts returns a filtered, paged listing ordered by most recently
// refreshed, along with the total number of matches.
func (p *Postgres) ListContracts(ctx context.Context, f ListFilter) (*ContractPage, error) {
	f.Normalize()

	// $1 empty means "any network", $2 empty means "any text" — expressing
	// both as SQL keeps this a single prepared statement rather than
	// concatenated fragments.
	// Symbols come from the spec the contract currently runs, so a function
	// removed by an upgrade stops matching.
	//
	// The exact-name filters read the jsonb rather than the symbols
	// column, because symbols flattens functions, types and events into
	// one list: it cannot say whether "transfer" is a function or a type.
	// Asking the jsonb keeps that distinction, and costs a scan of one
	// spec per row rather than a schema change.
	//
	// $2 also reaches the doc comments: a query matches either a name or
	// a word in the docs the GIN-indexed docs_tsv carries, one prefix
	// query per term at $11. The terms are the lowercase alphanumeric
	// words matchDocs splits out of the query for the memory store, so
	// both stores look for exactly the same words. Rows that match by
	// name rank as name_hit and sort ahead of doc-only matches.
	const q = `
		WITH matched AS (
			SELECT c.network, c.contract_id, c.current_wasm_hash, c.name, c.source_url, c.source_commit,
			       c.first_seen, c.last_refreshed, c.missing_since,
			       c.implements, c.deprecated_since, c.successor, c.deprecation_note,
			       c.creator, c.created_ledger, c.verified_by, c.verified_at, c.verified_wasm_hash,
			       COALESCE(s.symbols, '') AS symbols, COALESCE(s.docs_tsv, ''::tsvector) AS docs,
			       ($2 = '' OR c.contract_id ILIKE '%' || $2 || '%'
			               OR c.name ILIKE '%' || $2 || '%'
			               OR s.symbols ILIKE '%' || $2 || '%') AS name_hit
			FROM contracts c
			LEFT JOIN specs s
			  ON s.network = c.network AND s.contract_id = c.contract_id
			 AND s.wasm_hash = c.current_wasm_hash
			WHERE ($1 = '' OR c.network = $1)
			  AND ($2 = '' OR c.contract_id ILIKE '%' || $2 || '%'
			               OR c.name ILIKE '%' || $2 || '%'
			               OR s.symbols ILIKE '%' || $2 || '%'
			               OR (cardinality(COALESCE($11::text[], '{}'::text[])) > 0
			                   AND NOT EXISTS (
			                       SELECT 1
			                       FROM unnest(COALESCE($11::text[], '{}'::text[])) AS term(word)
			                       WHERE NOT (COALESCE(s.docs_tsv, ''::tsvector) @@ to_tsquery('simple', term.word || ':*')))))
			  AND NOT EXISTS (
			      SELECT 1 FROM unnest(COALESCE($3::text[], '{}'::text[])) AS want(name)
			      WHERE NOT EXISTS (
			          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'functions', '[]'::jsonb)) AS f
			          WHERE f->>'name' = want.name))
			  AND NOT EXISTS (
			      SELECT 1 FROM unnest(COALESCE($4::text[], '{}'::text[])) AS want(name)
			      WHERE NOT EXISTS (
			          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'types'->'structs', '[]'::jsonb)) AS t
			          WHERE t->>'name' = want.name
			          UNION ALL
			          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'types'->'unions', '[]'::jsonb)) AS t
			          WHERE t->>'name' = want.name
			          UNION ALL
			          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'types'->'enums', '[]'::jsonb)) AS t
			          WHERE t->>'name' = want.name
			          UNION ALL
			          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'types'->'error_enums', '[]'::jsonb)) AS t
			          WHERE t->>'name' = want.name))
			  AND (COALESCE(cardinality($5::text[]), 0) = 0 OR c.implements @> $5::text[])
		)
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit, first_seen, last_refreshed,
		       missing_since, implements, deprecated_since, successor, deprecation_note,
		       creator, created_ledger, verified_by, verified_at, verified_wasm_hash,
		       symbols, name_hit, count(*) OVER () AS total
		FROM matched
		WHERE ($8::timestamptz IS NULL
		       OR (CASE WHEN name_hit THEN 0 ELSE 1 END) > $12
		       OR ((CASE WHEN name_hit THEN 0 ELSE 1 END) = $12
		           AND (last_refreshed < $8
		                OR (last_refreshed = $8
		                    AND (contract_id > $9
		                         OR (contract_id = $9 AND network > $10))))))
		ORDER BY (CASE WHEN name_hit THEN 0 ELSE 1 END), last_refreshed DESC, contract_id, network
		LIMIT ($6+1) OFFSET $7`

	// The cursor pages by key: strictly after the row the cursor names,
	// in the very order the listing sorts. One more row than the page
	// needs is fetched so the answer can say whether a following page
	// exists; the extra row is dropped below.
	var cursorTime *time.Time
	var cursorID, cursorNetwork string
	var cursorTier int
	if f.Cursor != "" {
		if f.Offset != 0 {
			return nil, errors.New("store: a cursor and an offset are exclusive")
		}
		cur, err := decodeListCursor(f.Cursor)
		if err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, cur.LastRefreshed)
		if err != nil {
			return nil, fmt.Errorf("%w: bad timestamp %q", ErrMalformedCursor, cur.LastRefreshed)
		}
		cursorTime = &t
		cursorID, cursorNetwork = cur.ContractID, cur.Network
		// The SQL ranks a name hit 0 and a doc-only match 1; the cursor
		// carries the rank of the row it was minted from.
		cursorTier = cur.Rank
	}

	rows, err := p.pool.Query(ctx, q, f.Network, escapeLike(f.Query), f.Functions, f.Types, f.Implements, f.Limit, f.Offset,
		cursorTime, cursorID, cursorNetwork, docMatchLower(f.Query), cursorTier)
	if err != nil {
		return nil, fmt.Errorf("store: listing contracts: %w", err)
	}
	defer rows.Close()

	page := &ContractPage{Contracts: []Contract{}}
	fetched := []Contract{}
	ranks := []int{}
	for rows.Next() {
		var c Contract
		var symbols string
		var nameHit bool
		var total int
		if err := rows.Scan(
			&c.Network, &c.ContractID, &c.CurrentWasmHash, &c.Name, &c.SourceURL, &c.SourceCommit,
			&c.FirstSeen, &c.LastRefreshed, &c.MissingSince,
			&c.Implements, &c.DeprecatedSince, &c.Successor, &c.DeprecationNote,
			&c.Creator, &c.CreatedLedger, &c.VerifiedBy, &c.VerifiedAt, &c.VerifiedWasmHash,
			&symbols, &nameHit, &total,
		); err != nil {
			return nil, fmt.Errorf("store: scanning contract: %w", err)
		}
		// An unqueried listing computes name_hit true for every row, but
		// its rank is meaningless: it sorts on refresh time alone, and the
		// cursors it mints sit at the name tier the decode treats as 0.
		rank := 0
		if f.Query != "" {
			rank = docRank(nameHit)
		}
		c.Matches = matchSymbols(symbols, f.Query)
		page.Total = total
		fetched = append(fetched, c)
		ranks = append(ranks, rank)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing contracts: %w", err)
	}

	// The extra row proves a following page exists; everything the page
	// itself wants is the rows before it.
	if len(fetched) > f.Limit {
		fetched = fetched[:f.Limit]
		next, err := encodeListCursor(fetched[f.Limit-1], ranks[f.Limit-1])
		if err != nil {
			return nil, err
		}
		page.Next = next
	}
	if fetched != nil {
		page.Contracts = fetched
	}

	// The window function only reports a total on rows that came back, so a
	// page past the end needs a separate count to stay accurate.
	if len(page.Contracts) == 0 && (f.Offset > 0 || f.Cursor != "") {
		total, err := p.countContracts(ctx, f)
		if err != nil {
			return nil, err
		}
		page.Total = total
	}

	return page, nil
}

// ListContractsByWasm returns a paged listing of the contracts that run the
// given WASM hash, and — with Ever set — of the ones that ran it in the
// past.
//
// The "ever" case reads the specs table, which keeps one row per interface
// a contract has ever run; the join is on its unique key, so it costs an
// index lookup per candidate row.
func (p *Postgres) ListContractsByWasm(ctx context.Context, f WasmFilter) (*WasmContractPage, error) {
	f.Normalize()

	// $1 empty means "any network", $3 says whether the contracts that
	// only ran the hash in the past count too.
	const q = `
		SELECT c.network, c.contract_id, c.current_wasm_hash, c.name, c.source_url, c.source_commit,
		       c.first_seen, c.last_refreshed, c.missing_since,
		       c.implements, c.deprecated_since, c.successor, c.deprecation_note,
		       c.creator, c.created_ledger, c.verified_by, c.verified_at, c.verified_wasm_hash,
		       c.current_wasm_hash = $2 AS current,
		       count(*) OVER () AS total
		FROM contracts c
		WHERE ($1 = '' OR c.network = $1)
		  AND (c.current_wasm_hash = $2
		       OR ($3 AND EXISTS (
		           SELECT 1 FROM specs s
		           WHERE s.network = c.network AND s.contract_id = c.contract_id AND s.wasm_hash = $2)))
		ORDER BY c.last_refreshed DESC, c.contract_id
		LIMIT $4 OFFSET $5`

	rows, err := p.pool.Query(ctx, q, f.Network, f.WasmHash, f.Ever, f.Limit, f.Offset)
	if err != nil {
		return nil, fmt.Errorf("store: listing contracts for wasm %s: %w", f.WasmHash, err)
	}
	defer rows.Close()

	page := &WasmContractPage{Contracts: []WasmContract{}}
	for rows.Next() {
		var c WasmContract
		var total int
		if err := rows.Scan(
			&c.Network, &c.ContractID, &c.CurrentWasmHash, &c.Name, &c.SourceURL, &c.SourceCommit,
			&c.FirstSeen, &c.LastRefreshed, &c.MissingSince,
			&c.Implements, &c.DeprecatedSince, &c.Successor, &c.DeprecationNote,
			&c.Creator, &c.CreatedLedger, &c.VerifiedBy, &c.VerifiedAt, &c.VerifiedWasmHash,
			&c.Current, &total,
		); err != nil {
			return nil, fmt.Errorf("store: scanning contract: %w", err)
		}
		page.Contracts = append(page.Contracts, c)
		page.Total = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing contracts for wasm %s: %w", f.WasmHash, err)
	}

	// The window function only reports a total on rows that came back, so a
	// page past the end needs a separate count to stay accurate.
	if len(page.Contracts) == 0 && f.Offset > 0 {
		total, err := p.countContractsByWasm(ctx, f)
		if err != nil {
			return nil, err
		}
		page.Total = total
	}

	return page, nil
}

// countContractsByWasm counts the contracts that run a given WASM hash.
//
// It must mirror ListContractsByWasm's predicate, or a page past the end
// would report a different total from the pages before it.
func (p *Postgres) countContractsByWasm(ctx context.Context, f WasmFilter) (int, error) {
	const q = `
		SELECT count(*)
		FROM contracts c
		WHERE ($1 = '' OR c.network = $1)
		  AND (c.current_wasm_hash = $2
		       OR ($3 AND EXISTS (
		           SELECT 1 FROM specs s
		           WHERE s.network = c.network AND s.contract_id = c.contract_id AND s.wasm_hash = $2)))`

	var total int
	if err := p.pool.QueryRow(ctx, q, f.Network, f.WasmHash, f.Ever).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: counting contracts for wasm %s: %w", f.WasmHash, err)
	}
	return total, nil
}

// ListNetworks returns the distinct networks represented in the registry.
func (p *Postgres) ListNetworks(ctx context.Context) ([]string, error) {
	const q = `SELECT DISTINCT network FROM contracts ORDER BY network`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: listing networks: %w", err)
	}
	defer rows.Close()

	networks := []string{}
	for rows.Next() {
		var network string
		if err := rows.Scan(&network); err != nil {
			return nil, fmt.Errorf("store: scanning network: %w", err)
		}
		networks = append(networks, network)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing networks: %w", err)
	}
	return networks, nil
}

// Stats summarises the registry with two aggregate queries: one for the
// per-network counts, and one for the most recently registered contract.
func (p *Postgres) Stats(ctx context.Context) (*Stats, error) {
	const q = `
		WITH per_contract AS (
			SELECT c.network, c.contract_id, count(s.id) AS versions
			FROM contracts c
			LEFT JOIN specs s
			  ON s.network = c.network AND s.contract_id = c.contract_id
			GROUP BY c.network, c.contract_id
		)
		SELECT network, count(*) AS contracts, COALESCE(sum(versions), 0)::bigint AS versions
		FROM per_contract
		GROUP BY network
		ORDER BY network`

	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: counting registry: %w", err)
	}
	defer rows.Close()

	stats := &Stats{Networks: []NetworkStats{}}
	for rows.Next() {
		var entry NetworkStats
		if err := rows.Scan(&entry.Network, &entry.Contracts, &entry.Versions); err != nil {
			return nil, fmt.Errorf("store: scanning registry counts: %w", err)
		}
		stats.Networks = append(stats.Networks, entry)
		stats.Contracts += entry.Contracts
		stats.Versions += entry.Versions
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: counting registry: %w", err)
	}

	const latestQ = `
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit, first_seen, last_refreshed, missing_since,
		       creator, created_ledger, verified_by, verified_at, verified_wasm_hash
		FROM contracts
		ORDER BY first_seen DESC, contract_id
		LIMIT 1`

	var latest Contract
	err = p.pool.QueryRow(ctx, latestQ).Scan(
		&latest.Network, &latest.ContractID, &latest.CurrentWasmHash, &latest.Name,
		&latest.SourceURL, &latest.SourceCommit, &latest.FirstSeen, &latest.LastRefreshed,
		&latest.MissingSince,
		&latest.Creator, &latest.CreatedLedger, &latest.VerifiedBy, &latest.VerifiedAt, &latest.VerifiedWasmHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return stats, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading most recently registered contract: %w", err)
	}
	stats.Latest = &latest
	return stats, nil
}

// ListUpgrades returns recent interface upgrades, newest first.
//
// The window function pairs every spec row with the version its contract
// ran before it. Row one of each contract is its registration, so only rows
// two and later are upgrades.
func (p *Postgres) ListUpgrades(ctx context.Context, f UpgradeFilter) (*UpgradePage, error) {
	f.Normalize()

	const q = `
		WITH ordered AS (
			SELECT network, contract_id, wasm_hash, decoded_at,
			       lag(wasm_hash) OVER (
			           PARTITION BY network, contract_id
			           ORDER BY decoded_at, id
			       ) AS old_wasm_hash,
			       row_number() OVER (
			           PARTITION BY network, contract_id
			           ORDER BY decoded_at, id
			       ) AS position
			FROM specs
			WHERE ($1 = '' OR network = $1)
		)
		SELECT network, contract_id, old_wasm_hash, wasm_hash, decoded_at, count(*) OVER () AS total
		FROM ordered
		WHERE position > 1
		ORDER BY decoded_at DESC, contract_id, wasm_hash
		LIMIT $2 OFFSET $3`

	rows, err := p.pool.Query(ctx, q, f.Network, f.Limit, f.Offset)
	if err != nil {
		return nil, fmt.Errorf("store: listing upgrades: %w", err)
	}
	defer rows.Close()

	page := &UpgradePage{Upgrades: []Upgrade{}}
	for rows.Next() {
		var u Upgrade
		var total int
		if err := rows.Scan(&u.Network, &u.ContractID, &u.OldWasmHash, &u.NewWasmHash, &u.ObservedAt, &total); err != nil {
			return nil, fmt.Errorf("store: scanning upgrade: %w", err)
		}
		page.Upgrades = append(page.Upgrades, u)
		page.Total = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing upgrades: %w", err)
	}

	// The window function only reports a total on rows that came back, so
	// a page past the end needs the count on its own; otherwise it would
	// claim the registry has no upgrades at all.
	if len(page.Upgrades) == 0 && f.Offset > 0 {
		total, err := p.countUpgrades(ctx, f.Network)
		if err != nil {
			return nil, err
		}
		page.Total = total
	}

	return page, nil
}

// countUpgrades counts the spec rows beyond each contract's first, matching
// ListUpgrades' predicate.
func (p *Postgres) countUpgrades(ctx context.Context, network string) (int, error) {
	const q = `
		SELECT count(*)
		FROM (
			SELECT network, contract_id, wasm_hash,
			       row_number() OVER (
			           PARTITION BY network, contract_id
			           ORDER BY decoded_at, id
			       ) AS position
			FROM specs
			WHERE ($1 = '' OR network = $1)
		) ordered
		WHERE position > 1`

	var total int
	if err := p.pool.QueryRow(ctx, q, network).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: counting upgrades: %w", err)
	}
	return total, nil
}

func (p *Postgres) countContracts(ctx context.Context, f ListFilter) (int, error) {
	// Must mirror ListContracts' predicate, or a page past the end would
	// report a different total from the pages before it. $6 is the
	// doc-search term array the listing statement gets as $11.
	const q = `
		SELECT count(*)
		FROM contracts c
		LEFT JOIN specs s
		  ON s.network = c.network AND s.contract_id = c.contract_id
		 AND s.wasm_hash = c.current_wasm_hash
		WHERE ($1 = '' OR c.network = $1)
		  AND ($2 = '' OR c.contract_id ILIKE '%' || $2 || '%'
		               OR c.name ILIKE '%' || $2 || '%'
		               OR s.symbols ILIKE '%' || $2 || '%'
		               OR (cardinality(COALESCE($6::text[], '{}'::text[])) > 0
		                   AND NOT EXISTS (
		                       SELECT 1
		                       FROM unnest(COALESCE($6::text[], '{}'::text[])) AS term(word)
		                       WHERE NOT (COALESCE(s.docs_tsv, ''::tsvector) @@ to_tsquery('simple', term.word || ':*')))))
		  AND NOT EXISTS (
		      SELECT 1 FROM unnest(COALESCE($3::text[], '{}'::text[])) AS want(name)
		      WHERE NOT EXISTS (
		          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'functions', '[]'::jsonb)) AS f
		          WHERE f->>'name' = want.name))
		  AND NOT EXISTS (
		      SELECT 1 FROM unnest(COALESCE($4::text[], '{}'::text[])) AS want(name)
		      WHERE NOT EXISTS (
		          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'types'->'structs', '[]'::jsonb)) AS t
		          WHERE t->>'name' = want.name
		          UNION ALL
		          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'types'->'unions', '[]'::jsonb)) AS t
		          WHERE t->>'name' = want.name
		          UNION ALL
		          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'types'->'enums', '[]'::jsonb)) AS t
		          WHERE t->>'name' = want.name
		          UNION ALL
		          SELECT 1 FROM jsonb_array_elements(COALESCE(s.spec->'types'->'error_enums', '[]'::jsonb)) AS t
		          WHERE t->>'name' = want.name))
		  AND (COALESCE(cardinality($5::text[]), 0) = 0 OR c.implements @> $5::text[])`

	var total int
	if err := p.pool.QueryRow(ctx, q, f.Network, escapeLike(f.Query), f.Functions, f.Types, f.Implements, docMatchLower(f.Query)).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: counting contracts: %w", err)
	}
	return total, nil
}

// Subscribe creates a new webhook subscription for the given network and
// contract IDs. Returns the subscription ID (which also serves as the
// secret for HMAC signing and deletion).
func (p *Postgres) Subscribe(ctx context.Context, network string, contractIDs []string, url string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	id, err := newSubscriptionSecret()
	if err != nil {
		return "", err
	}

	const q = `
		INSERT INTO subscriptions (id, network, contract_ids, url, active, created_at)
		VALUES ($1, $2, $3, $4, true, now())
		ON CONFLICT (id) DO NOTHING
		RETURNING id`
	var insertedID string
	err = p.pool.QueryRow(ctx, q, id, network, strings.Join(contractIDs, ","), url).Scan(&insertedID)
	if err != nil {
		return "", fmt.Errorf("store: subscribing: %w", err)
	}
	return insertedID, nil
}

// ListSubscriptions returns all subscriptions, active and inactive.
func (p *Postgres) ListSubscriptions(ctx context.Context) ([]SubscriptionListItem, error) {
	const q = `
		SELECT id, network, contract_ids, active, created_at
		FROM subscriptions
		ORDER BY created_at DESC`

	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: listing subscriptions: %w", err)
	}
	defer rows.Close()

	items := []SubscriptionListItem{}
	for rows.Next() {
		var id, network string
		var contractIDsStr string
		var active bool
		var createdAt time.Time
		if err := rows.Scan(&id, &network, &contractIDsStr, &active, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scanning subscription: %w", err)
		}
		contractIDs := strings.Split(contractIDsStr, ",")
		if len(contractIDs) == 1 && contractIDs[0] == "" {
			contractIDs = nil
		}
		items = append(items, SubscriptionListItem{
			ID:        id,
			Network:   network,
			Contracts: len(contractIDs),
			Active:    active,
			CreatedAt: createdAt.UTC().Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing subscriptions: %w", err)
	}
	return items, nil
}

// GetSubscription returns a subscription by its secret (ID).
func (p *Postgres) GetSubscription(ctx context.Context, secret string) (*Subscription, error) {
	const q = `SELECT id, network, contract_ids, url, active, created_at FROM subscriptions WHERE id = $1`
	var id, network, contractIDsStr, url string
	var active bool
	var createdAt time.Time
	err := p.pool.QueryRow(ctx, q, secret).Scan(&id, &network, &contractIDsStr, &url, &active, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: subscription not found", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: getting subscription: %w", err)
	}
	contractIDs := strings.Split(contractIDsStr, ",")
	if len(contractIDs) == 1 && contractIDs[0] == "" {
		contractIDs = nil
	}
	sub := Subscription{
		ID:          id,
		Network:     network,
		ContractIDs: contractIDs,
		URL:         url,
		Active:      active,
		CreatedAt:   createdAt,
	}
	return &sub, nil
}

// MarkSubscriptionInactive marks a subscription as inactive.
func (p *Postgres) MarkSubscriptionInactive(ctx context.Context, secret string) error {
	const q = `UPDATE subscriptions SET active = false WHERE id = $1`
	tag, err := p.pool.Exec(ctx, q, secret)
	if err != nil {
		return fmt.Errorf("store: marking subscription inactive: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: subscription not found", ErrNotFound)
	}
	return nil
}

// Unsubscribe removes a subscription by its secret (ID).
func (p *Postgres) Unsubscribe(ctx context.Context, secret string) error {
	const q = `DELETE FROM subscriptions WHERE id = $1`
	tag, err := p.pool.Exec(ctx, q, secret)
	if err != nil {
		return fmt.Errorf("store: unsubscribing: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: subscription not found", ErrNotFound)
	}
	return nil
}

// GetSpec returns a stored interface. An empty wasmHash selects the version
// the contract currently runs.
func (p *Postgres) GetSpec(ctx context.Context, network, contractID, wasmHash string) (*Spec, error) {
	// Joining against contracts for the empty-hash case keeps "current"
	// defined by the contract row rather than by insertion order, which
	// matters if a contract is ever rolled back to an earlier WASM.
	const q = `
		SELECT s.id, s.network, s.contract_id, s.wasm_hash, s.spec, s.decoded_at, s.observed_ledger
		FROM specs s
		JOIN contracts c ON c.network = s.network AND c.contract_id = s.contract_id
		WHERE s.network = $1
		  AND s.contract_id = $2
		  AND s.wasm_hash = CASE WHEN $3 = '' THEN c.current_wasm_hash ELSE $3 END`

	var s Spec
	var body []byte
	err := p.pool.QueryRow(ctx, q, network, contractID, wasmHash).Scan(
		&s.ID, &s.Network, &s.ContractID, &s.WasmHash, &body, &s.DecodedAt, &s.ObservedLedger,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, specNotFound(network, contractID, wasmHash)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading spec for %s: %w", contractID, err)
	}

	var iface model.Interface
	if err := json.Unmarshal(body, &iface); err != nil {
		return nil, fmt.Errorf("store: decoding stored spec for %s: %w", contractID, err)
	}
	s.Interface = &iface

	return &s, nil
}

// ListVersions returns every stored version of a contract, newest first,
// flagging whichever one the contract currently runs.
func (p *Postgres) ListVersions(ctx context.Context, network, contractID string) ([]SpecVersion, error) {
	const q = `
		SELECT s.wasm_hash, s.decoded_at, s.wasm_hash = c.current_wasm_hash AS current, s.observed_ledger
		FROM specs s
		JOIN contracts c ON c.network = s.network AND c.contract_id = s.contract_id
		WHERE s.network = $1 AND s.contract_id = $2
		ORDER BY s.decoded_at DESC, s.id DESC`

	rows, err := p.pool.Query(ctx, q, network, contractID)
	if err != nil {
		return nil, fmt.Errorf("store: listing versions for %s: %w", contractID, err)
	}
	defer rows.Close()

	versions := []SpecVersion{}
	for rows.Next() {
		var v SpecVersion
		var versionID string
		if err := rows.Scan(&versionID, &v.DecodedAt, &v.Current, &v.ObservedLedger); err != nil {
			return nil, fmt.Errorf("store: scanning version: %w", err)
		}
		if versionID == model.StellarAssetContractIdentifier {
			v.InterfaceID = versionID
			v.Builtin = true
		} else {
			v.WasmHash = versionID
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing versions for %s: %w", contractID, err)
	}

	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return versions, nil
}

func specNotFound(network, contractID, wasmHash string) error {
	if wasmHash == "" {
		return fmt.Errorf("%w: spec for %s on %s", ErrNotFound, contractID, network)
	}
	return fmt.Errorf("%w: spec %s for %s on %s", ErrNotFound, wasmHash, contractID, network)
}

// escapeLike neutralises the wildcards in a user-supplied search term so that
// a query containing "%" or "_" matches those characters literally instead of
// silently matching far more than it appears to. Backslash is LIKE's default
// escape character, so it has to be doubled first.
//
// This is applied where the term meets SQL rather than in ListFilter, so that
// stores which match in Go still see the term the user typed.
func escapeLike(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, "%", `\%`)
	q = strings.ReplaceAll(q, "_", `\_`)
	return q
}
