package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/store"
)

// statusDeps returns dependencies for a database at the given version, with
// the given migrations embedded.
func statusDeps(version uint, dirty bool, migrations ...store.Migration) migrateStatusDeps {
	return migrateStatusDeps{
		Version: func(context.Context) (uint, bool, error) {
			return version, dirty, nil
		},
		Migrations: func(context.Context) ([]store.Migration, error) {
			return migrations, nil
		},
	}
}

// runMigrateStatusCmd runs the status command against the given dependencies
// and returns what it printed and the error it ended with.
func runMigrateStatusCmd(t *testing.T, deps migrateStatusDeps, args ...string) (string, error) {
	t.Helper()

	cmd := newMigrateStatusCmd(func() (migrateStatusDeps, error) { return deps, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return out.String(), err
}

// TestMigrateStatusReportsVersionsAndPendingMigrations proves the command
// says where the database is, where the binary is, and what is left to do.
func TestMigrateStatusReportsVersionsAndPendingMigrations(t *testing.T) {
	t.Parallel()

	deps := statusDeps(2, false,
		store.Migration{Version: 1, Name: "init"},
		store.Migration{Version: 2, Name: "spec_symbols"},
		store.Migration{Version: 3, Name: "wasm_specs"},
	)

	out, err := runMigrateStatusCmd(t, deps)
	require.Error(t, err, "pending migrations must end with a non-zero status")

	assert.Contains(t, out, "current version: 2")
	assert.Contains(t, out, "latest version:  3")
	assert.Contains(t, out, "wasm_specs", "a pending migration must be named")
	assert.NotContains(t, out, "spec_symbols", "an applied migration is not pending")
}

// TestMigrateStatusSucceedsWhenUpToDate proves a database with nothing
// pending exits zero.
func TestMigrateStatusSucceedsWhenUpToDate(t *testing.T) {
	t.Parallel()

	deps := statusDeps(3, false,
		store.Migration{Version: 1, Name: "init"},
		store.Migration{Version: 3, Name: "wasm_specs"},
	)

	out, err := runMigrateStatusCmd(t, deps)
	require.NoError(t, err, "an up-to-date database must exit zero")

	assert.Contains(t, out, "the database is up to date")
	assert.NotContains(t, out, "pending migrations")
}

// TestMigrateStatusReportsDirtyState proves a dirty database is reported
// with what to do about it, and ends with a non-zero status.
func TestMigrateStatusReportsDirtyState(t *testing.T) {
	t.Parallel()

	deps := statusDeps(2, true, store.Migration{Version: 1, Name: "init"})

	out, err := runMigrateStatusCmd(t, deps)
	require.Error(t, err, "a dirty database must exit non-zero")

	assert.Contains(t, out, "dirty")
	assert.Contains(t, out, "force <version>", "the report must say what to do about it")
	assert.Contains(t, out, "-database", "the report must say how to point the tool at the database")
}

// TestMigrateStatusReportsADatabaseAheadOfTheBinary proves a database
// migrated by a newer binary is reported rather than called up to date.
func TestMigrateStatusReportsADatabaseAheadOfTheBinary(t *testing.T) {
	t.Parallel()

	deps := statusDeps(5, false, store.Migration{Version: 4, Name: "contract_source"})

	out, err := runMigrateStatusCmd(t, deps)
	require.NoError(t, err, "there is nothing to apply")

	assert.Contains(t, out, "current version: 5")
	assert.Contains(t, out, "ahead of the migrations in this binary")
}

// TestMigrateStatusPrefersTheDirtyState proves a dirty database is reported
// as dirty even when migrations are pending as well: the schema is in an
// unknown state, so the pending list cannot be trusted either.
func TestMigrateStatusPrefersTheDirtyState(t *testing.T) {
	t.Parallel()

	deps := statusDeps(1, true,
		store.Migration{Version: 1, Name: "init"},
		store.Migration{Version: 2, Name: "spec_symbols"},
	)

	out, err := runMigrateStatusCmd(t, deps)
	require.Error(t, err, "a dirty database must exit non-zero")

	assert.Contains(t, out, "dirty")
	assert.NotContains(t, out, "pending migrations:",
		"a dirty database's pending list cannot be trusted")
}

// TestMigrateStatusFailsWithoutEmbeddedMigrations proves a binary that
// cannot know what "up to date" means says so, rather than certifying a
// database it knows nothing about.
func TestMigrateStatusFailsWithoutEmbeddedMigrations(t *testing.T) {
	t.Parallel()

	_, err := runMigrateStatusCmd(t, statusDeps(0, false))
	require.Error(t, err, "a binary with no migrations cannot certify a database")
	assert.Contains(t, err.Error(), "no migrations")
}

// TestMigrateStatusEmitsJSON proves the report can be read by a deployment
// gate rather than a person.
func TestMigrateStatusEmitsJSON(t *testing.T) {
	t.Parallel()

	deps := statusDeps(2, false,
		store.Migration{Version: 1, Name: "init"},
		store.Migration{Version: 3, Name: "wasm_specs"},
	)

	out, err := runMigrateStatusCmd(t, deps, "--json")
	require.Error(t, err, "pending migrations must end with a non-zero status")

	// The shape is spelled out here rather than reusing the command's own
	// type, so a renamed field is a failure rather than a silent change
	// of the wire format.
	var report struct {
		Current uint `json:"current_version"`
		Latest  uint `json:"latest_version"`
		Dirty   bool `json:"dirty"`
		Pending []struct {
			Version uint   `json:"version"`
			Name    string `json:"name"`
		} `json:"pending"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report), "the report must be JSON: %s", out)

	assert.Equal(t, uint(2), report.Current)
	assert.Equal(t, uint(3), report.Latest)
	assert.False(t, report.Dirty)
	require.Len(t, report.Pending, 1, "only the migration the database has not reached is pending")
	assert.Equal(t, uint(3), report.Pending[0].Version)
	assert.Equal(t, "wasm_specs", report.Pending[0].Name)
	assert.Contains(t, report.Message, "sorovault migrate",
		"the report must say what to do about pending migrations")
}

// TestMigrateStatusJSONExplainsADirtyDatabase proves the JSON report says
// what to do about a dirty database, and not only that it is dirty.
//
// The text report has said so since the command was written; a reader of
// the JSON deserves the same.
func TestMigrateStatusJSONExplainsADirtyDatabase(t *testing.T) {
	t.Parallel()

	deps := statusDeps(2, true,
		store.Migration{Version: 1, Name: "init"},
		store.Migration{Version: 3, Name: "wasm_specs"},
	)

	out, err := runMigrateStatusCmd(t, deps, "--json")
	require.Error(t, err, "a dirty database must exit non-zero")

	var report struct {
		Dirty   bool   `json:"dirty"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report), "the report must be JSON: %s", out)

	assert.True(t, report.Dirty, "the report must say the database is dirty")
	assert.Contains(t, report.Message, "dirty", "the report must say what is wrong")
	assert.Contains(t, report.Message, "force <version>",
		"the report must say what to do about it")
	assert.Contains(t, out, "force <version>",
		"the advice must be readable in the JSON, not escaped into \\u003cversion\\u003e")
}

// TestMigrateStatusRejectsArguments proves the command takes none.
func TestMigrateStatusRejectsArguments(t *testing.T) {
	t.Parallel()

	_, err := runMigrateStatusCmd(t, statusDeps(1, false), "extra")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

// TestMigrateStatusIsACommand proves `sorovault migrate status` is wired up,
// and that `sorovault migrate` still applies migrations.
func TestMigrateStatusIsACommand(t *testing.T) {
	t.Parallel()

	var migrate *cobra.Command
	for _, cmd := range newRootCmd().Commands() {
		if cmd.Name() == "migrate" {
			migrate = cmd
		}
	}
	require.NotNil(t, migrate, "sorovault migrate must exist")
	assert.True(t, migrate.Runnable(), "sorovault migrate must still apply migrations")

	var status *cobra.Command
	for _, cmd := range migrate.Commands() {
		if cmd.Name() == "status" {
			status = cmd
		}
	}
	require.NotNil(t, status, "sorovault migrate status must exist")
}

// TestMigrateStatusDoesNotApplyMigrations proves a status run applies
// nothing: the schema it is pointed at has no migration applied afterwards.
//
// golang-migrate creates the version table when it opens the database, so
// what is checked here is that no migration ran, not that the schema is
// untouched.
func TestMigrateStatusDoesNotApplyMigrations(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	schema := "migrate_status"
	createSchema(t, url, schema)
	t.Cleanup(func() { dropSchema(t, url, schema) })

	// A connection string that looks at the empty schema and nowhere else.
	scoped := url + "&options=-c%20search_path%3D" + schema

	deps := migrateStatusDeps{
		Version: func(context.Context) (uint, bool, error) {
			return store.MigrationVersion(scoped)
		},
		Migrations: func(context.Context) ([]store.Migration, error) {
			return store.EmbeddedMigrations()
		},
	}

	out, err := runMigrateStatusCmd(t, deps)
	require.Error(t, err, "a database with no migrations applied has pending migrations")
	assert.Contains(t, out, "current version: 0")

	counts := tableCounts(t, scoped)
	for _, table := range []string{"contracts", "specs", "wasm_specs"} {
		assert.NotContains(t, counts, table, "a status run must not apply migrations")
	}
}

// TestMigrateAppliesMigrations proves the migrate command still applies
// migrations now that it has a status subcommand.
func TestMigrateAppliesMigrations(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	schema := "migrate_apply"
	createSchema(t, url, schema)
	t.Cleanup(func() { dropSchema(t, url, schema) })

	scoped := url + "&options=-c%20search_path%3D" + schema
	t.Setenv("DATABASE_URL", scoped)
	t.Setenv("DATABASE_URL_FILE", "")

	cmd := newMigrateCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "database is at migration version")

	counts := tableCounts(t, scoped)
	assert.Contains(t, counts, "contracts", "the migrations should have been applied")
}

func TestMigrateAndStatusWithSQLiteDatabaseURL(t *testing.T) {
	url := store.SQLiteURLPrefix + filepath.ToSlash(filepath.Join(t.TempDir(), "registry.db"))
	t.Setenv("DATABASE_URL", url)
	t.Setenv("DATABASE_URL_FILE", "")

	// Before running migrate, status reports current version 0 and exits non-zero.
	statusCmd := newMigrateStatusCmd(migrateStatusDepsFromEnv)
	var statusBefore bytes.Buffer
	statusCmd.SetOut(&statusBefore)
	statusCmd.SetErr(&statusBefore)
	require.Error(t, statusCmd.Execute())
	assert.Contains(t, statusBefore.String(), "current version: 0")

	// Running `sorovault migrate` applies all SQLite migrations.
	migrateCmd := newMigrateCmd()
	var migrateOut bytes.Buffer
	migrateCmd.SetOut(&migrateOut)
	migrateCmd.SetErr(&migrateOut)
	require.NoError(t, migrateCmd.Execute())
	assert.Contains(t, migrateOut.String(), "database is at migration version")

	// After running migrate, status reports up to date and exits zero.
	statusCmdAfter := newMigrateStatusCmd(migrateStatusDepsFromEnv)
	var statusAfter bytes.Buffer
	statusCmdAfter.SetOut(&statusAfter)
	statusCmdAfter.SetErr(&statusAfter)
	require.NoError(t, statusCmdAfter.Execute())
	assert.Contains(t, statusAfter.String(), "the database is up to date")
}
