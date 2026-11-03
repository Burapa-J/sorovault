package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go database/sql driver registered as "sqlite"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/spec"
)

// SQLiteURLPrefix is the DATABASE_URL scheme prefix that selects the pure-Go
// SQLite backend. A URL of the form "sqlite:///path/to/registry.db" opens (or
// creates) the SQLite database file at "/path/to/registry.db".
const SQLiteURLPrefix = "sqlite://"

// sqliteTimeFormat formats UTC timestamps with a fixed 9-digit nanosecond
// fraction so lexicographical TEXT ordering in SQLite matches chronological
// time.Time ordering while remaining valid RFC3339Nano.
const sqliteTimeFormat = "2006-01-02T15:04:05.000000000Z"

// IsSQLiteURL reports whether databaseURL selects the SQLite backend.
func IsSQLiteURL(databaseURL string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(databaseURL)), SQLiteURLPrefix)
}

// Open connects to the store selected by databaseURL: SQLite when the URL
// begins with "sqlite://", and Postgres otherwise. PoolOptions apply to the
// Postgres connection pool and are ignored for SQLite.
func Open(ctx context.Context, databaseURL string, opts PoolOptions) (Store, error) {
	if IsSQLiteURL(databaseURL) {
		return NewSQLite(ctx, databaseURL)
	}
	return NewPostgres(ctx, databaseURL, opts)
}

// SQLite is the Store backed by a pure-Go SQLite database (modernc.org/sqlite)
// for single-user and local deployments.
//
// Search parity with Postgres:
//   - Postgres indexes doc comments in a GIN-backed tsvector column
//     (to_tsvector('simple', ...)) and evaluates ILIKE and jsonb_array_elements
//     predicates in SQL.
//   - SQLite stores the newline-joined symbol list (symbols), doc comments
//     (docs_tsv), and interface JSON (spec) as TEXT columns without requiring
//     FTS5 or C extensions, and evaluates case-insensitive substring matching
//     (matchSymbols), lowercased alphanumeric prefix word matching (matchDocs),
//     and exact function/type/standard filters in Go using the same rules as
//     the in-memory store.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// NewSQLite opens the SQLite database at databaseURL ("sqlite:///path/db" or
// "sqlite://:memory:"), configures WAL mode, busy_timeout and foreign key
// enforcement, and verifies the connection.
func NewSQLite(ctx context.Context, databaseURL string) (*SQLite, error) {
	db, err := openSQLiteDB(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &SQLite{db: db}, nil
}

func openSQLiteDB(ctx context.Context, databaseURL string) (*sql.DB, error) {
	_, dsn, err := parseSQLiteURL(databaseURL)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening sqlite: %w", err)
	}
	// Serialise access through a single connection so connection-scoped
	// pragmas stay in force and in-memory databases share state across
	// concurrent callers.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: pinging sqlite: %w", err)
	}

	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: configuring sqlite (%s): %w", pragma, err)
		}
	}

	return db, nil
}

// parseSQLiteURL converts a "sqlite://..." URL into a filesystem path (empty
// for in-memory databases) and a modernc.org/sqlite driver DSN.
func parseSQLiteURL(databaseURL string) (path string, dsn string, err error) {
	trimmed := strings.TrimSpace(databaseURL)
	if !IsSQLiteURL(trimmed) {
		return "", "", fmt.Errorf("store: invalid sqlite URL %q: must start with %s", databaseURL, SQLiteURLPrefix)
	}

	rest := trimmed[len(SQLiteURLPrefix):]
	rawPath, rawQuery, hasQuery := strings.Cut(rest, "?")
	if rawPath == "" {
		return "", "", fmt.Errorf("store: sqlite URL %q is missing a database file path", databaseURL)
	}

	var target string
	switch {
	case rawPath == ":memory:" || strings.HasPrefix(rawPath, "file::memory:"):
		target = rawPath
	default:
		cleaned := filepath.Clean(rawPath)
		if cleaned == "." || cleaned == "/" {
			return "", "", fmt.Errorf("store: sqlite URL %q does not name a database file", databaseURL)
		}
		dir := filepath.Dir(cleaned)
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			if statErr != nil {
				return "", "", fmt.Errorf("store: sqlite directory %q: %w", dir, statErr)
			}
			return "", "", fmt.Errorf("store: sqlite parent path %q is not a directory", dir)
		}
		path = cleaned
		target = cleaned
	}

	pragmas := "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	if hasQuery && rawQuery != "" {
		dsn = target + "?" + rawQuery + "&" + pragmas
	} else {
		dsn = target + "?" + pragmas
	}
	return path, dsn, nil
}

func pingSQLite(ctx context.Context, databaseURL string) error {
	db, err := openSQLiteDB(ctx, databaseURL)
	if err != nil {
		return err
	}
	return db.Close()
}

// Ping reports whether the SQLite database still answers.
func (s *SQLite) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: pinging sqlite: %w", err)
	}
	return nil
}

// Close closes the SQLite database handle.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// DB exposes the underlying *sql.DB handle for schema inspection in tests.
func (s *SQLite) DB() *sql.DB {
	return s.db
}

// SaveWasmSpec stores an interface for uploaded code independent of contracts.
func (s *SQLite) SaveWasmSpec(ctx context.Context, network, wasmHash string, iface *model.Interface) error {
	if iface == nil {
		return errors.New("store: cannot save a nil interface")
	}
	body, err := json.Marshal(iface)
	if err != nil {
		return fmt.Errorf("store: encoding interface: %w", err)
	}
	now := formatSQLiteTime(time.Now().UTC())
	const q = `
		INSERT INTO wasm_specs (network, wasm_hash, spec, decoded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (network, wasm_hash) DO UPDATE SET
			spec       = excluded.spec,
			decoded_at = excluded.decoded_at`
	if _, err := s.db.ExecContext(ctx, q, network, wasmHash, string(body), now); err != nil {
		return fmt.Errorf("store: saving wasm spec %s: %w", wasmHash, err)
	}
	return nil
}

// GetWasmSpec returns a hash-addressed interface without requiring a contract.
func (s *SQLite) GetWasmSpec(ctx context.Context, network, wasmHash string) (*WasmSpec, error) {
	const q = `SELECT network, wasm_hash, spec, decoded_at FROM wasm_specs WHERE network = ? AND wasm_hash = ?`
	var out WasmSpec
	var body, decodedAt string
	err := s.db.QueryRowContext(ctx, q, network, wasmHash).Scan(&out.Network, &out.WasmHash, &body, &decodedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: wasm spec %s on %s", ErrNotFound, wasmHash, network)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading wasm spec %s: %w", wasmHash, err)
	}
	t, err := parseSQLiteTime(decodedAt)
	if err != nil {
		return nil, fmt.Errorf("store: parsing decoded_at for wasm spec %s: %w", wasmHash, err)
	}
	out.DecodedAt = t

	var iface model.Interface
	if err := json.Unmarshal([]byte(body), &iface); err != nil {
		return nil, fmt.Errorf("store: decoding wasm spec %s: %w", wasmHash, err)
	}
	out.Interface = &iface
	return &out, nil
}

// Save upserts the contract and appends its spec version in one transaction.
func (s *SQLite) Save(ctx context.Context, c Contract, iface *model.Interface) error {
	if iface == nil {
		return errors.New("store: cannot save a nil interface")
	}

	body, err := json.Marshal(iface)
	if err != nil {
		return fmt.Errorf("store: encoding interface: %w", err)
	}
	c.Implements = spec.ImplementedStandards(iface)
	implementsJSON, err := encodeStringSlice(c.Implements)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the tx is committed

	now := formatSQLiteTime(time.Now().UTC())

	const upsertContract = `
		INSERT INTO contracts (network, contract_id, current_wasm_hash, name, implements, first_seen, last_refreshed)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, contract_id) DO UPDATE SET
			current_wasm_hash = excluded.current_wasm_hash,
			name              = CASE WHEN excluded.name <> '' THEN excluded.name ELSE contracts.name END,
			implements        = excluded.implements,
			last_refreshed    = excluded.last_refreshed,
			missing_since     = NULL`

	if _, err := tx.ExecContext(ctx, upsertContract,
		c.Network, c.ContractID, c.CurrentWasmHash, c.Name, implementsJSON, now, now); err != nil {
		return fmt.Errorf("store: saving contract %s: %w", c.ContractID, err)
	}

	var observed any
	if c.ObservedLedger != nil {
		observed = int64(*c.ObservedLedger)
	}

	const insertSpec = `
		INSERT INTO specs (network, contract_id, wasm_hash, spec, symbols, docs_tsv, decoded_at, observed_ledger)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, contract_id, wasm_hash) DO UPDATE SET
			spec     = excluded.spec,
			symbols  = excluded.symbols,
			docs_tsv = excluded.docs_tsv`

	if _, err := tx.ExecContext(ctx, insertSpec,
		c.Network, c.ContractID, c.CurrentWasmHash, string(body),
		joinSymbols(iface), joinDocs(iface), now, observed); err != nil {
		return fmt.Errorf("store: saving spec for %s: %w", c.ContractID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// GetContract returns one contract's current state.
func (s *SQLite) GetContract(ctx context.Context, network, contractID string) (*Contract, error) {
	const q = `
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit,
		       first_seen, last_refreshed, missing_since,
		       implements, deprecated_since, successor, deprecation_note,
		       creator, created_ledger, verified_by, verified_at, verified_wasm_hash
		FROM contracts
		WHERE network = ? AND contract_id = ?`

	c, err := scanSQLiteContract(s.db.QueryRowContext(ctx, q, network, contractID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading contract %s: %w", contractID, err)
	}
	return &c, nil
}

// MarkMissing records that a contract could not be found on the network.
func (s *SQLite) MarkMissing(ctx context.Context, network, contractID string, since time.Time) error {
	const q = `
		UPDATE contracts SET missing_since = COALESCE(missing_since, ?)
		WHERE network = ? AND contract_id = ?`

	res, err := s.db.ExecContext(ctx, q, formatSQLiteTime(since.UTC()), network, contractID)
	if err != nil {
		return fmt.Errorf("store: marking contract %s missing: %w", contractID, err)
	}
	return requireOneRow(res, network, contractID)
}

// SetDeprecation marks an existing contract deprecated.
func (s *SQLite) SetDeprecation(ctx context.Context, network, contractID, successor, note string) error {
	const q = `
		UPDATE contracts
		SET deprecated_since = ?, successor = ?, deprecation_note = ?
		WHERE network = ? AND contract_id = ?`
	res, err := s.db.ExecContext(ctx, q, formatSQLiteTime(time.Now().UTC()), successor, note, network, contractID)
	if err != nil {
		return fmt.Errorf("store: deprecating contract %s: %w", contractID, err)
	}
	return requireOneRow(res, network, contractID)
}

// ClearDeprecation lifts the deprecation mark on a contract.
func (s *SQLite) ClearDeprecation(ctx context.Context, network, contractID string) error {
	const q = `
		UPDATE contracts
		SET deprecated_since = NULL, successor = '', deprecation_note = ''
		WHERE network = ? AND contract_id = ?`
	res, err := s.db.ExecContext(ctx, q, network, contractID)
	if err != nil {
		return fmt.Errorf("store: clearing deprecation for %s: %w", contractID, err)
	}
	return requireOneRow(res, network, contractID)
}

// UpdateSource attaches repository metadata to an existing contract.
func (s *SQLite) UpdateSource(ctx context.Context, network, contractID, sourceURL, sourceCommit string) error {
	const q = `UPDATE contracts SET source_url = ?, source_commit = ? WHERE network = ? AND contract_id = ?`
	res, err := s.db.ExecContext(ctx, q, sourceURL, sourceCommit, network, contractID)
	if err != nil {
		return fmt.Errorf("store: updating source for %s: %w", contractID, err)
	}
	return requireOneRow(res, network, contractID)
}

// SetOrigin records who created a contract and in which ledger.
func (s *SQLite) SetOrigin(ctx context.Context, network, contractID, creator string, ledger uint32) error {
	const q = `UPDATE contracts SET creator = ?, created_ledger = ? WHERE network = ? AND contract_id = ?`
	res, err := s.db.ExecContext(ctx, q, creator, int64(ledger), network, contractID)
	if err != nil {
		return fmt.Errorf("store: recording origin of %s: %w", contractID, err)
	}
	return requireOneRow(res, network, contractID)
}

// SetVerified records an accepted publisher claim.
func (s *SQLite) SetVerified(ctx context.Context, network, contractID, name, sourceURL, sourceCommit string, v Verification) error {
	const q = `
		UPDATE contracts SET
			name = ?, source_url = ?, source_commit = ?,
			verified_by = ?, verified_at = ?, verified_wasm_hash = ?
		WHERE network = ? AND contract_id = ?`
	res, err := s.db.ExecContext(ctx, q, name, sourceURL, sourceCommit,
		v.Publisher, formatSQLiteTime(v.VerifiedAt.UTC()), v.WasmHash, network, contractID)
	if err != nil {
		return fmt.Errorf("store: verifying %s: %w", contractID, err)
	}
	return requireOneRow(res, network, contractID)
}

// GetState returns a persisted value.
func (s *SQLite) GetState(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: state %q", ErrNotFound, key)
	}
	if err != nil {
		return "", fmt.Errorf("store: reading state %q: %w", key, err)
	}
	return value, nil
}

// SetState stores a value under key.
func (s *SQLite) SetState(ctx context.Context, key, value string) error {
	const q = `
		INSERT INTO state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
	if _, err := s.db.ExecContext(ctx, q, key, value, formatSQLiteTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("store: writing state %q: %w", key, err)
	}
	return nil
}

// Delete removes a contract and every version stored for it.
func (s *SQLite) Delete(ctx context.Context, network, contractID string) error {
	const q = `DELETE FROM contracts WHERE network = ? AND contract_id = ?`
	res, err := s.db.ExecContext(ctx, q, network, contractID)
	if err != nil {
		return fmt.Errorf("store: deleting contract %s: %w", contractID, err)
	}
	return requireOneRow(res, network, contractID)
}

// Export returns every contract and interface version in a stable order.
func (s *SQLite) Export(ctx context.Context) (*Snapshot, error) {
	const contractsQuery = `
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit,
		       first_seen, last_refreshed, missing_since,
		       implements, deprecated_since, successor, deprecation_note,
		       creator, created_ledger, verified_by, verified_at, verified_wasm_hash
		FROM contracts
		ORDER BY network, contract_id`

	rows, err := s.db.QueryContext(ctx, contractsQuery)
	if err != nil {
		return nil, fmt.Errorf("store: exporting contracts: %w", err)
	}
	defer rows.Close()

	snapshot := &Snapshot{Version: SnapshotVersion, Contracts: []ContractSnapshot{}}
	index := make(map[string]int)
	for rows.Next() {
		c, scanErr := scanSQLiteContract(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scanning exported contract: %w", scanErr)
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
	specRows, err := s.db.QueryContext(ctx, specsQuery)
	if err != nil {
		return nil, fmt.Errorf("store: exporting specs: %w", err)
	}
	defer specRows.Close()

	for specRows.Next() {
		var network, contractID, wasmHash, body, decodedAtRaw string
		if err := specRows.Scan(&network, &contractID, &wasmHash, &body, &decodedAtRaw); err != nil {
			return nil, fmt.Errorf("store: scanning exported spec: %w", err)
		}
		decodedAt, err := parseSQLiteTime(decodedAtRaw)
		if err != nil {
			return nil, fmt.Errorf("store: parsing exported spec timestamp: %w", err)
		}
		var iface model.Interface
		if err := json.Unmarshal([]byte(body), &iface); err != nil {
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
func (s *SQLite) Import(ctx context.Context, snapshot *Snapshot, merge bool) error {
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin import: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the tx is committed

	if !merge {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM contracts`).Scan(&count); err != nil {
			return fmt.Errorf("store: checking registry is empty: %w", err)
		}
		if count > 0 {
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, contract_id) DO NOTHING`

	const insertSpec = `
		INSERT INTO specs (network, contract_id, wasm_hash, spec, symbols, docs_tsv, decoded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, contract_id, wasm_hash) DO NOTHING`

	for _, cs := range snapshot.Contracts {
		c := cs.Contract
		implementsJSON, err := encodeStringSlice(c.Implements)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, insertContract,
			c.Network, c.ContractID, c.CurrentWasmHash, c.Name, c.SourceURL, c.SourceCommit,
			formatSQLiteTime(c.FirstSeen), formatSQLiteTime(c.LastRefreshed), formatOptionalSQLiteTime(c.MissingSince),
			implementsJSON, formatOptionalSQLiteTime(c.DeprecatedSince), c.Successor, c.DeprecationNote,
			c.Creator, int64(c.CreatedLedger), c.VerifiedBy, formatOptionalSQLiteTime(c.VerifiedAt), c.VerifiedWasmHash); err != nil {
			return fmt.Errorf("store: importing contract %s: %w", c.ContractID, err)
		}

		for _, version := range cs.Versions {
			body, err := json.Marshal(version.Interface)
			if err != nil {
				return fmt.Errorf("store: encoding imported spec for %s: %w", c.ContractID, err)
			}
			if _, err := tx.ExecContext(ctx, insertSpec,
				c.Network, c.ContractID, version.WasmHash, string(body),
				joinSymbols(version.Interface), joinDocs(version.Interface),
				formatSQLiteTime(version.DecodedAt)); err != nil {
				return fmt.Errorf("store: importing spec for %s: %w", c.ContractID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit import: %w", err)
	}
	return nil
}

// ListContracts returns a filtered, paged listing ordered by search rank and
// most recently refreshed, along with the total number of matches.
func (s *SQLite) ListContracts(ctx context.Context, f ListFilter) (*ContractPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Normalize()
	if f.Cursor != "" && f.Offset != 0 {
		return nil, errors.New("store: a cursor and an offset are exclusive")
	}
	var cur listCursor
	if f.Cursor != "" {
		var err error
		cur, err = decodeListCursor(f.Cursor)
		if err != nil {
			return nil, err
		}
	}

	const q = `
		SELECT c.network, c.contract_id, c.current_wasm_hash, c.name, c.source_url, c.source_commit,
		       c.first_seen, c.last_refreshed, c.missing_since,
		       c.implements, c.deprecated_since, c.successor, c.deprecation_note,
		       c.creator, c.created_ledger, c.verified_by, c.verified_at, c.verified_wasm_hash,
		       COALESCE(s.spec, ''), COALESCE(s.symbols, ''), COALESCE(s.docs_tsv, '')
		FROM contracts c
		LEFT JOIN specs s
		  ON s.network = c.network AND s.contract_id = c.contract_id
		 AND s.wasm_hash = c.current_wasm_hash
		WHERE (? = '' OR c.network = ?)`

	dbRows, err := s.db.QueryContext(ctx, q, f.Network, f.Network)
	if err != nil {
		return nil, fmt.Errorf("store: listing contracts: %w", err)
	}
	defer dbRows.Close()

	type docRow struct {
		contract Contract
		rank     int
	}
	var rows []docRow
	needle := strings.ToLower(f.Query)

	for dbRows.Next() {
		c, specBody, symbols, docs, scanErr := scanSQLiteContractWithSpec(dbRows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scanning contract: %w", scanErr)
		}
		if len(f.Implements) > 0 && !hasAllStandards(c.Implements, f.Implements) {
			continue
		}
		if len(f.Functions) > 0 || len(f.Types) > 0 {
			var iface *model.Interface
			if specBody != "" {
				var parsed model.Interface
				if err := json.Unmarshal([]byte(specBody), &parsed); err != nil {
					return nil, fmt.Errorf("store: decoding spec for %s: %w", c.ContractID, err)
				}
				iface = &parsed
			}
			if !hasAll(iface, f.Functions, f.Types) {
				continue
			}
		}

		rank := 0
		if needle != "" {
			c.Matches = matchSymbols(symbols, f.Query)
			nameHit := len(c.Matches) > 0 ||
				strings.Contains(strings.ToLower(c.ContractID), needle) ||
				strings.Contains(strings.ToLower(c.Name), needle)
			if !nameHit && !matchDocs(docs, f.Query) {
				continue
			}
			rank = docRank(nameHit)
		}
		rows = append(rows, docRow{contract: c, rank: rank})
	}
	if err := dbRows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing contracts: %w", err)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].rank != rows[j].rank {
			return rows[i].rank < rows[j].rank
		}
		return contractListBefore(rows[i].contract, rows[j].contract)
	})

	start := 0
	if cur.ContractID != "" {
		start = sort.Search(len(rows), func(i int) bool {
			return afterListCursor(rows[i].contract, rows[i].rank, cur)
		})
	} else {
		start = f.Offset
	}

	page := &ContractPage{Total: len(rows), Contracts: []Contract{}}
	if start < len(rows) {
		end := min(start+f.Limit, len(rows))
		for _, r := range rows[start:end] {
			page.Contracts = append(page.Contracts, r.contract)
		}
		if end < len(rows) {
			next, err := encodeListCursor(rows[end-1].contract, rows[end-1].rank)
			if err != nil {
				return nil, err
			}
			page.Next = next
		}
	}
	return page, nil
}

// ListContractsByWasm returns a paged listing of contracts that run (or with
// Ever set, have ever run) the given WASM hash.
func (s *SQLite) ListContractsByWasm(ctx context.Context, f WasmFilter) (*WasmContractPage, error) {
	f.Normalize()

	const q = `
		SELECT c.network, c.contract_id, c.current_wasm_hash, c.name, c.source_url, c.source_commit,
		       c.first_seen, c.last_refreshed, c.missing_since,
		       c.implements, c.deprecated_since, c.successor, c.deprecation_note,
		       c.creator, c.created_ledger, c.verified_by, c.verified_at, c.verified_wasm_hash
		FROM contracts c
		WHERE (? = '' OR c.network = ?)
		  AND (c.current_wasm_hash = ?
		       OR (? = 1 AND EXISTS (
		           SELECT 1 FROM specs s
		           WHERE s.network = c.network AND s.contract_id = c.contract_id AND s.wasm_hash = ?)))
		ORDER BY c.last_refreshed DESC, c.contract_id`

	everInt := 0
	if f.Ever {
		everInt = 1
	}
	rows, err := s.db.QueryContext(ctx, q, f.Network, f.Network, f.WasmHash, everInt, f.WasmHash)
	if err != nil {
		return nil, fmt.Errorf("store: listing contracts for wasm %s: %w", f.WasmHash, err)
	}
	defer rows.Close()

	var matched []WasmContract
	for rows.Next() {
		c, scanErr := scanSQLiteContract(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scanning contract: %w", scanErr)
		}
		matched = append(matched, WasmContract{
			Contract: c,
			Current:  c.CurrentWasmHash == f.WasmHash,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing contracts for wasm %s: %w", f.WasmHash, err)
	}

	page := &WasmContractPage{Total: len(matched), Contracts: []WasmContract{}}
	if f.Offset < len(matched) {
		end := min(f.Offset+f.Limit, len(matched))
		page.Contracts = append(page.Contracts, matched[f.Offset:end]...)
	}
	return page, nil
}

// ListNetworks returns the distinct networks represented in the registry.
func (s *SQLite) ListNetworks(ctx context.Context) ([]string, error) {
	const q = `SELECT DISTINCT network FROM contracts ORDER BY network`
	rows, err := s.db.QueryContext(ctx, q)
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

// Stats summarises the registry with aggregate counts per network and the most
// recently registered contract.
func (s *SQLite) Stats(ctx context.Context) (*Stats, error) {
	const q = `
		WITH per_contract AS (
			SELECT c.network, c.contract_id, count(s.id) AS versions
			FROM contracts c
			LEFT JOIN specs s
			  ON s.network = c.network AND s.contract_id = c.contract_id
			GROUP BY c.network, c.contract_id
		)
		SELECT network, count(*) AS contracts, COALESCE(sum(versions), 0) AS versions
		FROM per_contract
		GROUP BY network
		ORDER BY network`

	rows, err := s.db.QueryContext(ctx, q)
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
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit,
		       first_seen, last_refreshed, missing_since,
		       implements, deprecated_since, successor, deprecation_note,
		       creator, created_ledger, verified_by, verified_at, verified_wasm_hash
		FROM contracts
		ORDER BY first_seen DESC, contract_id
		LIMIT 1`

	latest, err := scanSQLiteContract(s.db.QueryRowContext(ctx, latestQ))
	if errors.Is(err, sql.ErrNoRows) {
		return stats, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading most recently registered contract: %w", err)
	}
	// Match Postgres Stats.Latest projection (which leaves Implements and
	// deprecation fields at their zero values on Stats.Latest).
	latest.Implements = nil
	latest.DeprecatedSince = nil
	latest.Successor = ""
	latest.DeprecationNote = ""
	stats.Latest = &latest
	return stats, nil
}

// ListUpgrades returns recorded interface upgrades, newest first.
func (s *SQLite) ListUpgrades(ctx context.Context, f UpgradeFilter) (*UpgradePage, error) {
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
			WHERE (? = '' OR network = ?)
		)
		SELECT network, contract_id, old_wasm_hash, wasm_hash, decoded_at
		FROM ordered
		WHERE position > 1
		ORDER BY decoded_at DESC, contract_id, wasm_hash`

	rows, err := s.db.QueryContext(ctx, q, f.Network, f.Network)
	if err != nil {
		return nil, fmt.Errorf("store: listing upgrades: %w", err)
	}
	defer rows.Close()

	var upgrades []Upgrade
	for rows.Next() {
		var u Upgrade
		var decodedAtRaw string
		if err := rows.Scan(&u.Network, &u.ContractID, &u.OldWasmHash, &u.NewWasmHash, &decodedAtRaw); err != nil {
			return nil, fmt.Errorf("store: scanning upgrade: %w", err)
		}
		t, err := parseSQLiteTime(decodedAtRaw)
		if err != nil {
			return nil, fmt.Errorf("store: parsing upgrade timestamp: %w", err)
		}
		u.ObservedAt = t
		upgrades = append(upgrades, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing upgrades: %w", err)
	}

	page := &UpgradePage{Total: len(upgrades), Upgrades: []Upgrade{}}
	if f.Offset < len(upgrades) {
		end := min(f.Offset+f.Limit, len(upgrades))
		page.Upgrades = append(page.Upgrades, upgrades[f.Offset:end]...)
	}
	return page, nil
}

// Subscribe creates a new webhook subscription for the given network and
// contract IDs.
func (s *SQLite) Subscribe(ctx context.Context, network string, contractIDs []string, url string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	id, err := newSubscriptionSecret()
	if err != nil {
		return "", err
	}

	const q = `
		INSERT INTO subscriptions (id, network, contract_ids, url, active, created_at)
		VALUES (?, ?, ?, ?, 1, ?)`
	if _, err := s.db.ExecContext(ctx, q, id, network, strings.Join(contractIDs, ","), url, formatSQLiteTime(time.Now().UTC())); err != nil {
		return "", fmt.Errorf("store: subscribing: %w", err)
	}
	return id, nil
}

// ListSubscriptions returns all subscriptions, active and inactive.
func (s *SQLite) ListSubscriptions(ctx context.Context) ([]SubscriptionListItem, error) {
	const q = `
		SELECT id, network, contract_ids, active, created_at
		FROM subscriptions
		ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: listing subscriptions: %w", err)
	}
	defer rows.Close()

	items := []SubscriptionListItem{}
	for rows.Next() {
		var id, network, contractIDsStr, createdAtRaw string
		var activeInt int
		if err := rows.Scan(&id, &network, &contractIDsStr, &activeInt, &createdAtRaw); err != nil {
			return nil, fmt.Errorf("store: scanning subscription: %w", err)
		}
		createdAt, err := parseSQLiteTime(createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("store: parsing subscription timestamp: %w", err)
		}
		contractIDs := strings.Split(contractIDsStr, ",")
		if len(contractIDs) == 1 && contractIDs[0] == "" {
			contractIDs = nil
		}
		items = append(items, SubscriptionListItem{
			ID:        id,
			Network:   network,
			Contracts: len(contractIDs),
			Active:    activeInt != 0,
			CreatedAt: createdAt.UTC().Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing subscriptions: %w", err)
	}
	return items, nil
}

// GetSubscription returns a subscription by its secret (ID).
func (s *SQLite) GetSubscription(ctx context.Context, secret string) (*Subscription, error) {
	const q = `SELECT id, network, contract_ids, url, active, created_at FROM subscriptions WHERE id = ?`
	var id, network, contractIDsStr, url, createdAtRaw string
	var activeInt int
	err := s.db.QueryRowContext(ctx, q, secret).Scan(&id, &network, &contractIDsStr, &url, &activeInt, &createdAtRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: subscription not found", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: getting subscription: %w", err)
	}
	createdAt, err := parseSQLiteTime(createdAtRaw)
	if err != nil {
		return nil, fmt.Errorf("store: parsing subscription timestamp: %w", err)
	}
	contractIDs := strings.Split(contractIDsStr, ",")
	if len(contractIDs) == 1 && contractIDs[0] == "" {
		contractIDs = nil
	}
	return &Subscription{
		ID:          id,
		Network:     network,
		ContractIDs: contractIDs,
		URL:         url,
		Active:      activeInt != 0,
		CreatedAt:   createdAt,
	}, nil
}

// MarkSubscriptionInactive marks a subscription as inactive.
func (s *SQLite) MarkSubscriptionInactive(ctx context.Context, secret string) error {
	const q = `UPDATE subscriptions SET active = 0 WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, secret)
	if err != nil {
		return fmt.Errorf("store: marking subscription inactive: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: marking subscription inactive: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("%w: subscription not found", ErrNotFound)
	}
	return nil
}

// Unsubscribe removes a subscription by its secret (ID).
func (s *SQLite) Unsubscribe(ctx context.Context, secret string) error {
	const q = `DELETE FROM subscriptions WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, secret)
	if err != nil {
		return fmt.Errorf("store: unsubscribing: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: unsubscribing: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("%w: subscription not found", ErrNotFound)
	}
	return nil
}

// GetSpec returns a stored interface. An empty wasmHash selects the version
// the contract currently runs.
func (s *SQLite) GetSpec(ctx context.Context, network, contractID, wasmHash string) (*Spec, error) {
	const q = `
		SELECT s.id, s.network, s.contract_id, s.wasm_hash, s.spec, s.decoded_at, s.observed_ledger
		FROM specs s
		JOIN contracts c ON c.network = s.network AND c.contract_id = s.contract_id
		WHERE s.network = ?
		  AND s.contract_id = ?
		  AND s.wasm_hash = CASE WHEN ? = '' THEN c.current_wasm_hash ELSE ? END`

	var out Spec
	var body, decodedAtRaw string
	var observed sql.NullInt64
	err := s.db.QueryRowContext(ctx, q, network, contractID, wasmHash, wasmHash).Scan(
		&out.ID, &out.Network, &out.ContractID, &out.WasmHash, &body, &decodedAtRaw, &observed,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, specNotFound(network, contractID, wasmHash)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading spec for %s: %w", contractID, err)
	}

	decodedAt, err := parseSQLiteTime(decodedAtRaw)
	if err != nil {
		return nil, fmt.Errorf("store: parsing spec timestamp for %s: %w", contractID, err)
	}
	out.DecodedAt = decodedAt
	if observed.Valid {
		led := uint32(observed.Int64)
		out.ObservedLedger = &led
	}

	var iface model.Interface
	if err := json.Unmarshal([]byte(body), &iface); err != nil {
		return nil, fmt.Errorf("store: decoding stored spec for %s: %w", contractID, err)
	}
	out.Interface = &iface
	return &out, nil
}

// ListVersions returns every stored version of a contract, newest first,
// flagging whichever one the contract currently runs.
func (s *SQLite) ListVersions(ctx context.Context, network, contractID string) ([]SpecVersion, error) {
	const q = `
		SELECT s.wasm_hash, s.decoded_at, s.wasm_hash = c.current_wasm_hash AS current, s.observed_ledger
		FROM specs s
		JOIN contracts c ON c.network = s.network AND c.contract_id = s.contract_id
		WHERE s.network = ? AND s.contract_id = ?
		ORDER BY s.decoded_at DESC, s.id DESC`

	rows, err := s.db.QueryContext(ctx, q, network, contractID)
	if err != nil {
		return nil, fmt.Errorf("store: listing versions for %s: %w", contractID, err)
	}
	defer rows.Close()

	versions := []SpecVersion{}
	for rows.Next() {
		var v SpecVersion
		var versionID, decodedAtRaw string
		var observed sql.NullInt64
		if err := rows.Scan(&versionID, &decodedAtRaw, &v.Current, &observed); err != nil {
			return nil, fmt.Errorf("store: scanning version: %w", err)
		}
		decodedAt, err := parseSQLiteTime(decodedAtRaw)
		if err != nil {
			return nil, fmt.Errorf("store: parsing version timestamp: %w", err)
		}
		v.DecodedAt = decodedAt
		if observed.Valid {
			led := uint32(observed.Int64)
			v.ObservedLedger = &led
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

type sqliteRowScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteContract(row sqliteRowScanner) (Contract, error) {
	var (
		c                                   Contract
		firstSeenRaw, lastRefreshedRaw      string
		missingSinceRaw, deprecatedSinceRaw sql.NullString
		verifiedAtRaw                       sql.NullString
		implementsRaw                       string
		createdLedger                       int64
	)
	if err := row.Scan(
		&c.Network, &c.ContractID, &c.CurrentWasmHash, &c.Name, &c.SourceURL, &c.SourceCommit,
		&firstSeenRaw, &lastRefreshedRaw, &missingSinceRaw,
		&implementsRaw, &deprecatedSinceRaw, &c.Successor, &c.DeprecationNote,
		&c.Creator, &createdLedger, &c.VerifiedBy, &verifiedAtRaw, &c.VerifiedWasmHash,
	); err != nil {
		return Contract{}, err
	}
	return populateSQLiteContract(c, firstSeenRaw, lastRefreshedRaw, missingSinceRaw, implementsRaw, deprecatedSinceRaw, createdLedger, verifiedAtRaw)
}

func scanSQLiteContractWithSpec(row sqliteRowScanner) (Contract, string, string, string, error) {
	var (
		c                                   Contract
		firstSeenRaw, lastRefreshedRaw      string
		missingSinceRaw, deprecatedSinceRaw sql.NullString
		verifiedAtRaw                       sql.NullString
		implementsRaw                       string
		createdLedger                       int64
		specBody, symbols, docs             string
	)
	if err := row.Scan(
		&c.Network, &c.ContractID, &c.CurrentWasmHash, &c.Name, &c.SourceURL, &c.SourceCommit,
		&firstSeenRaw, &lastRefreshedRaw, &missingSinceRaw,
		&implementsRaw, &deprecatedSinceRaw, &c.Successor, &c.DeprecationNote,
		&c.Creator, &createdLedger, &c.VerifiedBy, &verifiedAtRaw, &c.VerifiedWasmHash,
		&specBody, &symbols, &docs,
	); err != nil {
		return Contract{}, "", "", "", err
	}
	populated, err := populateSQLiteContract(c, firstSeenRaw, lastRefreshedRaw, missingSinceRaw, implementsRaw, deprecatedSinceRaw, createdLedger, verifiedAtRaw)
	if err != nil {
		return Contract{}, "", "", "", err
	}
	return populated, specBody, symbols, docs, nil
}

func populateSQLiteContract(
	c Contract,
	firstSeenRaw, lastRefreshedRaw string,
	missingSinceRaw sql.NullString,
	implementsRaw string,
	deprecatedSinceRaw sql.NullString,
	createdLedger int64,
	verifiedAtRaw sql.NullString,
) (Contract, error) {
	firstSeen, err := parseSQLiteTime(firstSeenRaw)
	if err != nil {
		return Contract{}, fmt.Errorf("first_seen: %w", err)
	}
	lastRefreshed, err := parseSQLiteTime(lastRefreshedRaw)
	if err != nil {
		return Contract{}, fmt.Errorf("last_refreshed: %w", err)
	}
	c.FirstSeen = firstSeen
	c.LastRefreshed = lastRefreshed

	if missingSinceRaw.Valid && missingSinceRaw.String != "" {
		t, err := parseSQLiteTime(missingSinceRaw.String)
		if err != nil {
			return Contract{}, fmt.Errorf("missing_since: %w", err)
		}
		c.MissingSince = &t
	}
	if deprecatedSinceRaw.Valid && deprecatedSinceRaw.String != "" {
		t, err := parseSQLiteTime(deprecatedSinceRaw.String)
		if err != nil {
			return Contract{}, fmt.Errorf("deprecated_since: %w", err)
		}
		c.DeprecatedSince = &t
	}
	if verifiedAtRaw.Valid && verifiedAtRaw.String != "" {
		t, err := parseSQLiteTime(verifiedAtRaw.String)
		if err != nil {
			return Contract{}, fmt.Errorf("verified_at: %w", err)
		}
		c.VerifiedAt = &t
	}

	implements, err := decodeStringSlice(implementsRaw)
	if err != nil {
		return Contract{}, err
	}
	c.Implements = implements
	c.CreatedLedger = uint32(createdLedger)
	return c, nil
}

func formatSQLiteTime(t time.Time) string {
	return t.UTC().Format(sqliteTimeFormat)
}

func formatOptionalSQLiteTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatSQLiteTime(*t)
}

func parseSQLiteTime(raw string) (time.Time, error) {
	for _, layout := range []string{
		sqliteTimeFormat,
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", raw)
}

func encodeStringSlice(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("store: encoding string slice: %w", err)
	}
	return string(b), nil
}

func decodeStringSlice(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("store: decoding string slice %q: %w", raw, err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func requireOneRow(res sql.Result, network, contractID string) error {
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: checking rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}
