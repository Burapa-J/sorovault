package store_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault"
	"github.com/soroworks/sorovault/internal/store"
)

// newSQLiteStore returns a migrated, empty SQLite store backed by a temp file.
//
// Unlike Postgres, this requires no external server — the database file lives
// under the test temp directory — so the shared store conformance suite runs
// against SQLite in a plain `go test ./...`.
func newSQLiteStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()

	url := sqliteFileURL(t)
	require.NoError(t, store.Migrate(url), "migrating sqlite test database")

	s, err := store.NewSQLite(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

// sqliteFileURL builds a sqlite:///path/registry.db DATABASE_URL in t.TempDir().
func sqliteFileURL(t *testing.T) string {
	t.Helper()
	return store.SQLiteURLPrefix + filepath.ToSlash(filepath.Join(t.TempDir(), "registry.db"))
}

func TestOpenSelectsSQLiteFromDatabaseURL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	url := sqliteFileURL(t)
	require.NoError(t, store.Migrate(url))
	require.NoError(t, store.Ping(ctx, url))

	s, err := store.Open(ctx, url, store.PoolOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	_, ok := s.(*store.SQLite)
	assert.True(t, ok, "DATABASE_URL=sqlite:///path/registry.db must open the pure-Go SQLite store")
	require.NoError(t, s.Ping(ctx))

	require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))
	got, err := s.GetContract(ctx, "testnet", idA)
	require.NoError(t, err)
	assert.Equal(t, idA, got.ContractID)

	// Non-sqlite:// URLs preserve the Postgres path and its eager connection check.
	_, err = store.Open(ctx, "postgres://sorovault:sorovault@127.0.0.1:1/none?sslmode=disable", store.PoolOptions{})
	require.Error(t, err)
}

func TestNewSQLiteRejectsBadURLs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, err := store.NewSQLite(ctx, "")
	require.ErrorContains(t, err, "sqlite://")

	_, err = store.NewSQLite(ctx, "postgres://localhost:5432/sorovault")
	require.ErrorContains(t, err, "sqlite://")

	_, err = store.NewSQLite(ctx, "sqlite://")
	require.ErrorContains(t, err, "missing a database file path")

	_, err = store.NewSQLite(ctx, "sqlite:///nonexistent-parent-dir-sorovault-200/registry.db")
	require.ErrorContains(t, err, "sqlite directory")
}

func TestSQLiteMigrationsAreReversibleAndIdempotent(t *testing.T) {
	t.Parallel()
	url := sqliteFileURL(t)

	// Before any migration runs, MigrationStatus is strictly read-only and must
	// not create the SQLite file on disk.
	rawPath := strings.TrimPrefix(url, store.SQLiteURLPrefix)
	embedded, err := store.EmbeddedMigrationsFor(url)
	require.NoError(t, err)
	require.NotEmpty(t, embedded)
	latestVersion := embedded[len(embedded)-1].Version

	ver, latest, dirty, err := store.MigrationStatus(context.Background(), url)
	require.NoError(t, err)
	assert.Equal(t, uint(0), ver)
	assert.Equal(t, latestVersion, latest)
	assert.False(t, dirty)
	_, statErr := os.Stat(rawPath)
	assert.True(t, os.IsNotExist(statErr), "MigrationStatus must not create a missing SQLite file")

	// Apply all migrations twice to verify idempotency.
	require.NoError(t, store.Migrate(url))
	require.NoError(t, store.Migrate(url))

	ver, latest, dirty, err = store.MigrationStatus(context.Background(), url)
	require.NoError(t, err)
	assert.False(t, dirty, "migrations must not leave the database dirty")
	assert.Equal(t, latestVersion, ver)
	assert.Equal(t, latestVersion, latest)

	// Roll back every migration and re-apply.
	require.NoError(t, store.MigrateDown(url))
	ver, latest, dirty, err = store.MigrationStatus(context.Background(), url)
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.Equal(t, uint(0), ver)
	assert.Equal(t, latestVersion, latest)

	require.NoError(t, store.Migrate(url))
	ver, latest, dirty, err = store.MigrationStatus(context.Background(), url)
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.Equal(t, latestVersion, ver)
	assert.Equal(t, latestVersion, latest)
}

// --- dialect parity -------------------------------------------------------
//
// Both migration sets (Postgres in migrations/*.sql and SQLite in
// migrations/sqlite/*.sql) must describe the same logical schema: same
// migration versions and names, same tables, same columns per table, and
// matching live SQLite table info after running store.Migrate.

func TestMigrationDialectsStayInParity(t *testing.T) {
	t.Parallel()

	pgEmbedded, err := store.EmbeddedMigrations()
	require.NoError(t, err)
	sqliteEmbedded, err := store.EmbeddedSQLiteMigrations()
	require.NoError(t, err)
	require.Equal(t, pgEmbedded, sqliteEmbedded, "Postgres and SQLite migration version/name lists must stay in parity")

	pgVersions := migrationVersions(t, "migrations")
	sqliteVersions := migrationVersions(t, "migrations/sqlite")
	require.Equal(t, pgVersions, sqliteVersions, "every migration must exist in both dialects with paired up/down files")

	pgTables := dialectSchema(t, "migrations")
	sqliteTables := dialectSchema(t, "migrations/sqlite")

	require.Equal(t, setOfKeys(pgTables), setOfKeys(sqliteTables), "table sets diverged between Postgres and SQLite migrations")
	for _, table := range []string{"contracts", "specs", "wasm_specs", "subscriptions", "state"} {
		require.Contains(t, pgTables, table, "missing expected table %s", table)
		assert.Equal(t,
			setOf(pgTables[table]),
			setOf(sqliteTables[table]),
			"columns diverged between Postgres and SQLite migrations for table %s", table,
		)
	}

	// Verify the parsed schema against the actual schema SQLite produces after store.Migrate.
	live := liveSQLiteSchema(t)
	assert.Equal(t, setOfKeys(pgTables), setOfKeys(live), "live SQLite tables differ from Postgres migrations")
	for table, cols := range pgTables {
		assert.Equal(t, setOf(cols), setOf(live[table]), "live SQLite columns differ from Postgres migrations for table %s", table)
	}
}

func dialectSchema(t *testing.T, dir string) map[string][]string {
	t.Helper()

	files, err := fs.Glob(sorovault.Migrations, dir+"/*.up.sql")
	require.NoError(t, err)
	sort.Strings(files)
	require.NotEmpty(t, files, "dialect %s has no up migrations", dir)

	tables := map[string][]string{}
	createRE := regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\((.*?)\n\)`)
	dropRE := regexp.MustCompile(`(?i)DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	renameRE := regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)\s+RENAME\s+TO\s+(\w+)`)

	for _, file := range files {
		raw, err := sorovault.Migrations.ReadFile(file)
		require.NoError(t, err)
		sqlText := string(raw)

		for _, m := range createRE.FindAllStringSubmatch(sqlText, -1) {
			tables[strings.ToLower(m[1])] = parseColumns(t, m[2])
		}
		for _, m := range renameRE.FindAllStringSubmatch(sqlText, -1) {
			oldName := strings.ToLower(m[1])
			newName := strings.ToLower(m[2])
			if cols, ok := tables[oldName]; ok {
				delete(tables, oldName)
				tables[newName] = cols
			}
		}
		for _, m := range dropRE.FindAllStringSubmatch(sqlText, -1) {
			name := strings.ToLower(m[1])
			if renamedInto(sqlText, name) {
				continue
			}
			delete(tables, name)
		}
		applyColumnAlters(tables, sqlText)
	}
	return tables
}

var (
	alterTableRE = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(\w+)\s+(.*)$`)
	addColumnRE  = regexp.MustCompile(`(?i)\bADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)
	dropColumnRE = regexp.MustCompile(`(?i)\bDROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	renameToRE   = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+\w+\s+RENAME\s+TO\s+(\w+)`)
)

func applyColumnAlters(tables map[string][]string, sqlText string) {
	var body strings.Builder
	for _, line := range strings.Split(sqlText, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	for _, stmt := range strings.Split(body.String(), ";") {
		m := alterTableRE.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		table := strings.ToLower(m[1])
		for _, add := range addColumnRE.FindAllStringSubmatch(m[2], -1) {
			col := strings.ToLower(add[1])
			if !containsString(tables[table], col) {
				tables[table] = append(tables[table], col)
			}
		}
		for _, drop := range dropColumnRE.FindAllStringSubmatch(m[2], -1) {
			col := strings.ToLower(drop[1])
			kept := tables[table][:0]
			for _, c := range tables[table] {
				if c != col {
					kept = append(kept, c)
				}
			}
			tables[table] = kept
		}
	}
}

func renamedInto(sqlText, name string) bool {
	for _, m := range renameToRE.FindAllStringSubmatch(sqlText, -1) {
		if strings.EqualFold(m[1], name) {
			return true
		}
	}
	return false
}

var skipLinePrefixes = []string{"constraint", "primary", "unique", "check", "foreign"}

func parseColumns(t *testing.T, body string) []string {
	t.Helper()

	var cols []string
	var entry strings.Builder
	balance := 0

	flush := func() {
		line := strings.TrimSpace(entry.String())
		entry.Reset()
		line = strings.TrimSuffix(line, ",")
		if line == "" || strings.HasPrefix(line, "--") {
			return
		}
		fields := strings.Fields(line)
		name := strings.ToLower(strings.Trim(fields[0], `"`))
		for _, p := range skipLinePrefixes {
			if name == p {
				return
			}
		}
		cols = append(cols, name)
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if idx := strings.Index(trimmed, "--"); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
		if trimmed == "" && balance == 0 {
			continue
		}
		entry.WriteString(trimmed)
		entry.WriteString("\n")
		balance += strings.Count(trimmed, "(") - strings.Count(trimmed, ")")
		if balance <= 0 {
			balance = 0
			flush()
		}
	}
	require.Equal(t, 0, balance, "unbalanced parentheses in migration body: %s", body)
	return cols
}

func liveSQLiteSchema(t *testing.T) map[string][]string {
	t.Helper()

	ctx := context.Background()
	raw := newSQLiteStore(t)
	db := raw.(*store.SQLite).DB()

	out := map[string][]string{}
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations'
		ORDER BY name`)
	require.NoError(t, err)
	var tableNames []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tableNames = append(tableNames, n)
	}
	require.NoError(t, rows.Close())

	for _, table := range tableNames {
		pr, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		require.NoError(t, err)
		for pr.Next() {
			var n string
			require.NoError(t, pr.Scan(&n))
			out[strings.ToLower(table)] = append(out[strings.ToLower(table)], strings.ToLower(n))
		}
		require.NoError(t, pr.Close())
	}
	return out
}

func migrationVersions(t *testing.T, dir string) []string {
	t.Helper()

	files, err := fs.Glob(sorovault.Migrations, dir+"/*.up.sql")
	require.NoError(t, err)
	var names []string
	for _, f := range files {
		base := filepath.Base(f)
		names = append(names, strings.TrimSuffix(base, ".up.sql"))
	}
	sort.Strings(names)

	down, err := fs.Glob(sorovault.Migrations, dir+"/*.down.sql")
	require.NoError(t, err)
	var downNames []string
	for _, f := range down {
		base := filepath.Base(f)
		downNames = append(downNames, strings.TrimSuffix(base, ".down.sql"))
	}
	sort.Strings(downNames)
	require.Equal(t, names, downNames, "every up migration needs a paired down in %s", dir)
	return names
}

func setOf(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[strings.ToLower(it)] = true
	}
	return out
}

func setOfKeys(m map[string][]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[strings.ToLower(k)] = true
	}
	return out
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
