package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/soroworks/sorovault/internal/codegen"
	"github.com/soroworks/sorovault/internal/config"
	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/refresher"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
)

func newAddCmd() *cobra.Command {
	var (
		asJSON   bool
		fromFile string
	)

	cmd := &cobra.Command{
		Use:   "add <contract_id>...",
		Short: "Fetch, decode and register one or more contracts",
		Long: `Fetch each contract's WASM from the network, decode the interface
embedded in it, and store the result.

Adding a contract that is already registered is not an error: it re-checks
the contract against the network and records a new interface version if the
contract has been upgraded since it was last seen.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && fromFile == "" {
				return fmt.Errorf("at least one contract ID is required, or use --from-file")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			contractIDs, err := loadContractIDs(args, fromFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			if len(contractIDs) == 0 {
				return fmt.Errorf("no contract IDs found")
			}

			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				var failed int

				for _, contractID := range contractIDs {
					fetchCtx, cancel := a.fetchContext(ctx)
					result, err := a.registry.Register(fetchCtx, contractID)
					cancel()

					if err != nil {
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", contractID, err)
						failed++
						continue
					}

					if asJSON {
						if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
							return err
						}
						continue
					}
					printRegisterResult(cmd.OutOrStdout(), result)
				}

				if failed > 0 {
					return fmt.Errorf("%w: %d of %d contract(s) failed", errPrinted, failed, len(contractIDs))
				}
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the full result as JSON")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read contract IDs from a file (one per line; use - for stdin)")
	return cmd
}

func loadContractIDs(args []string, path string, input io.Reader) ([]string, error) {
	ids := append([]string(nil), args...)
	if path == "" {
		return ids, nil
	}

	reader := input
	if path != "-" {
		// #nosec G304 -- the CLI operator explicitly selects this input file.
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("opening contract ID file %s: %w", path, err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading contract ID file %s: %w", path, err)
	}
	return ids, nil
}

func newRemoveCmd() *cobra.Command {
	var (
		network string
		yes     bool
	)

	cmd := &cobra.Command{
		Use:   "remove <contract_id>",
		Short: "Delete a contract and all its stored versions",
		Long: `Delete a contract from the registry, together with every interface
version stored for it on the selected network.

The API deliberately has no route for this: it is unauthenticated, so
removal is an operator action available only through the CLI. Without --yes
the command asks for confirmation before deleting anything.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				contractID := args[0]

				net := network
				if net == "" {
					var err error
					if net, err = a.registry.Network(ctx); err != nil {
						return err
					}
				}

				if !yes {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Remove %s from %s and all its versions? [y/N] ", contractID, net)
					if !confirmed(cmd.InOrStdin()) {
						_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
						return nil
					}
				}

				if err := a.registry.Store().Delete(ctx, net, contractID); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", contractID, net)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network to remove from (default: the network RPC_URL serves)")
	cmd.Flags().BoolVar(&yes, "yes", false, "delete without asking for confirmation")
	return cmd
}

// confirmed reads one answer and accepts only an explicit yes.
func confirmed(r io.Reader) bool {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func newExportCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "export [file]",
		Short: "Write the registry as versioned JSON",
		Long: `Write every contract and every stored interface version, with their
timestamps, as one versioned JSON document. With no file, or with "-", it goes
to standard output.

The document is portable between databases: "sorovault import" loads it
without contacting RPC.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && output != "" {
				return fmt.Errorf("give either a file argument or --output, not both")
			}
			path := output
			if len(args) == 1 {
				path = args[0]
			}

			return withStore(cmd.Context(), func(ctx context.Context, s store.Store) error {
				snapshot, err := s.Export(ctx)
				if err != nil {
					return err
				}

				if path == "" || path == "-" {
					return writeJSON(cmd.OutOrStdout(), snapshot)
				}

				file, err := os.Create(path) //nolint:gosec // the path is the operator's --output argument
				if err != nil {
					return fmt.Errorf("creating %s: %w", path, err)
				}
				if err := writeJSON(file, snapshot); err != nil {
					_ = file.Close()
					return fmt.Errorf("writing %s: %w", path, err)
				}
				if err := file.Close(); err != nil {
					return fmt.Errorf("writing %s: %w", path, err)
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d contract(s))\n", path, len(snapshot.Contracts))
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of stdout")
	return cmd
}

func newImportCmd() *cobra.Command {
	var (
		input string
		merge bool
	)

	cmd := &cobra.Command{
		Use:   "import [file]",
		Short: "Load a registry exported as JSON",
		Long: `Load a document written by "sorovault export" into the registry.

Without --merge the registry must be empty; the import then reproduces every
contract, interface version and timestamp exactly. With --merge only the
contracts and versions that are missing are added, and records already in the
database are left alone.

Import reads the document directly into the store. It never contacts RPC, so
it works when the network is unavailable.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && input != "" {
				return fmt.Errorf("give either a file argument or --input, not both")
			}
			path := input
			if len(args) == 1 {
				path = args[0]
			}

			var data []byte
			var err error
			if path == "" || path == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(path) //nolint:gosec // the path is the operator's --input argument
			}
			if err != nil {
				return fmt.Errorf("reading snapshot: %w", err)
			}

			var snapshot store.Snapshot
			if err := json.Unmarshal(data, &snapshot); err != nil {
				return fmt.Errorf("reading snapshot: %w", err)
			}

			return withStore(cmd.Context(), func(ctx context.Context, s store.Store) error {
				if err := s.Import(ctx, &snapshot, merge); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "imported %d contract(s)\n", len(snapshot.Contracts))
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&input, "input", "i", "", "read from this file instead of stdin")
	cmd.Flags().BoolVar(&merge, "merge", false, "add only what is missing instead of requiring an empty registry")
	return cmd
}

// printSweepSummary writes one line describing a refresh sweep.
func printSweepSummary(w io.Writer, sum refresher.Summary) {
	_, _ = fmt.Fprintf(w, "checked %d, changed %d, missing %d, failed %d, busy %d\n",
		sum.Checked, sum.Changed, sum.Missing, sum.Failed, sum.Busy)
}

func newRefreshCmd() *cobra.Command {
	var (
		asJSON   bool
		all      bool
		fromFile string
	)

	cmd := &cobra.Command{
		Use:   "refresh [contract_id]...",
		Short: "Re-check registered contracts for an upgraded interface",
		Long: `Compare each registered contract against the network and record a new
interface version if its WASM has changed.

With --all it runs one sweep over every contract on the network, regardless
of how recently each was refreshed, then exits. It is meant for a cron job or
for catching up after an RPC outage; it does not wait an interval.

Unlike "add", refresh starts from what is already stored, so an unchanged
contract costs a single RPC round trip: the module is only downloaded once
its hash is known to differ.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if all {
				if len(args) > 0 || fromFile != "" {
					return fmt.Errorf("--all refreshes every contract and cannot be combined with contract IDs or --from-file")
				}
				return nil
			}
			if len(args) == 0 && fromFile == "" {
				return fmt.Errorf("at least one contract ID is required, or use --from-file or --all")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			contractIDs := args
			if !all {
				var err error
				contractIDs, err = loadContractIDs(args, fromFile, cmd.InOrStdin())
				if err != nil {
					return err
				}
				if len(contractIDs) == 0 {
					return fmt.Errorf("no contract IDs found")
				}
			}

			if all {
				return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
					interval := a.cfg.RefreshInterval
					if interval <= 0 {
						// SweepAll ignores staleness and pacing; the interval
						// only satisfies New's positive-duration guard.
						interval = time.Minute
					}

					r, err := refresher.New(a.registry, refresher.Options{
						Interval: interval,
						Timeout:  a.cfg.RPCTimeout,
						Logger:   a.log.With("component", "refresher"),
					})
					if err != nil {
						return err
					}

					summary, err := r.SweepAll(ctx)
					if err != nil {
						return err
					}

					if asJSON {
						if err := writeJSON(cmd.OutOrStdout(), summary); err != nil {
							return err
						}
					} else {
						printSweepSummary(cmd.OutOrStdout(), summary)
					}

					if summary.Failed > 0 {
						return fmt.Errorf("%w: %d contract(s) failed to refresh", errPrinted, summary.Failed)
					}
					return nil
				})
			}

			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				var failed int

				for _, contractID := range contractIDs {
					fetchCtx, cancel := a.fetchContext(ctx)
					result, err := a.registry.Refresh(fetchCtx, contractID)
					cancel()

					if err != nil {
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", contractID, err)
						failed++
						continue
					}

					if asJSON {
						if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
							return err
						}
						continue
					}

					if result.Changed {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s upgraded to %s\n",
							result.Contract.ContractID, result.Contract.CurrentWasmHash)
						continue
					}
					if result.Missing {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s missing from the network\n",
							result.Contract.ContractID)
						continue
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s unchanged\n", result.Contract.ContractID)
				}

				if failed > 0 {
					return fmt.Errorf("%w: %d of %d contract(s) failed", errPrinted, failed, len(contractIDs))
				}
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the full result as JSON")
	cmd.Flags().BoolVar(&all, "all", false, "run one sweep over every contract on the network")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read contract IDs from a file (one per line; use - for stdin)")
	registerContractIDCompletion(cmd, "all")
	return cmd
}

func newListCmd() *cobra.Command {
	var (
		query     string
		network   string
		functions []string
		types     []string
		limit     int
		offset    int
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered contracts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				filter := store.ListFilter{
					Query:     query,
					Network:   network,
					Functions: functions,
					Types:     types,
					Limit:     limit,
					Offset:    offset,
				}

				page, err := a.registry.Store().ListContracts(ctx, filter)
				if err != nil {
					return err
				}

				if asJSON {
					return writeJSON(cmd.OutOrStdout(), page)
				}

				if len(page.Contracts) == 0 {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "no contracts registered")
					return nil
				}

				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "CONTRACT ID\tNETWORK\tWASM HASH\tLAST REFRESHED\tMATCHES\tSTATUS")
				for _, c := range page.Contracts {
					status := ""
					if c.MissingSince != nil {
						status = "missing"
					}
					if c.Verified() {
						status = strings.TrimSpace(status + " verified")
					}
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
						c.ContractID, c.Network, truncate(c.CurrentWasmHash, 12),
						c.LastRefreshed.UTC().Format("2006-01-02 15:04"),
						strings.Join(c.Matches, ","), status)
				}
				if err := tw.Flush(); err != nil {
					return err
				}

				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "\n%d of %d contract(s)\n", len(page.Contracts), page.Total)
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&query, "query", "q", "", "search contract ID, name, and function, type and event names")
	cmd.Flags().StringVar(&network, "network", "", "restrict to one network")
	cmd.Flags().StringArrayVar(&functions, "function", nil, "only contracts with a function of exactly this name (repeatable)")
	cmd.Flags().StringArrayVar(&types, "type", nil, "only contracts with a user-defined type of exactly this name (repeatable)")
	cmd.Flags().IntVar(&limit, "limit", store.DefaultLimit, "maximum rows to return")
	cmd.Flags().IntVar(&offset, "offset", 0, "rows to skip")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit results as JSON")
	return cmd
}

func newGetCmd() *cobra.Command {
	var (
		network  string
		wasmHash string
		function string
		asJSON   bool
	)

	cmd := &cobra.Command{
		Use:   "get <contract_id>",
		Short: "Show a registered contract's decoded interface",
		Long: `Print a contract's decoded interface.

With --json the output is the same ABI document the HTTP API serves, so it
can be piped straight into other tooling.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				contractID := args[0]

				net := network
				if net == "" {
					var err error
					if net, err = a.registry.Network(ctx); err != nil {
						return err
					}
				}

				stored, err := a.registry.Store().GetSpec(ctx, net, contractID, wasmHash)
				if err != nil {
					return err
				}

				if function != "" {
					fn, ok := stored.Interface.Function(function)
					if !ok {
						return fmt.Errorf("no function %q in contract %s", function, contractID)
					}
					if asJSON {
						return writeJSON(cmd.OutOrStdout(), fn)
					}
					printFunction(cmd.OutOrStdout(), fn)
					return nil
				}

				if asJSON {
					return writeJSON(cmd.OutOrStdout(), stored.Interface)
				}
				printInterface(cmd.OutOrStdout(), stored)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network to read from (default: the network RPC_URL serves)")
	cmd.Flags().StringVar(&wasmHash, "wasm-hash", "", "show a specific version instead of the current one")
	cmd.Flags().StringVar(&function, "function", "", "show only this function")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the ABI as JSON")
	registerContractIDCompletion(cmd)
	registerNetworkCompletion(cmd)
	return cmd
}

func newVersionsCmd() *cobra.Command {
	var (
		network string
		asJSON  bool
	)

	cmd := &cobra.Command{
		Use:   "versions <contract_id>",
		Short: "List a contract's stored interface versions",
		Long: `Print every interface SoroVault has stored for a contract, newest first.

The current version is the one the contract runs now; an older one can be
read with "get --wasm-hash".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				net := network
				if net == "" {
					var err error
					if net, err = a.registry.Network(ctx); err != nil {
						return err
					}
				}

				versions, err := a.registry.Store().ListVersions(ctx, net, args[0])
				if err != nil {
					return err
				}

				if asJSON {
					return writeJSON(cmd.OutOrStdout(), versionsDocument{
						ContractID: args[0],
						Network:    net,
						Versions:   versions,
					})
				}
				printVersions(cmd.OutOrStdout(), versions)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network to read from (default: the network RPC_URL serves)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the versions as JSON")
	registerContractIDCompletion(cmd)
	registerNetworkCompletion(cmd)
	return cmd
}

func newCodegenCmd() *cobra.Command {
	var (
		network     string
		wasmHash    string
		lang        string
		output      string
		withZod     bool
		asPackage   bool
		packageName string
		pkgVersion  string
	)

	cmd := &cobra.Command{
		Use:   "codegen <contract_id>",
		Short: "Generate a typed client for a registered contract",
		Long: `Generate a typed client from a registered contract's stored interface.

The client is generated from what SoroVault already holds, so no network
access is needed. --wasm-hash generates against a superseded version.

Supported formats are TypeScript (ts), Go (go), Python (py), Markdown (md)
and example argument lists (examples): one example argument object per
function, keyed by function name. With --lang go the output is a Go client
that builds on github.com/stellar/go-stellar-sdk, one method per function
returning an invocation operation; --name sets its package name. With
--lang py the output is a Python client that builds on stellar-sdk, one
method per function returning the SDK's AssembledTransaction. With --package
the output is a complete npm package: the client under src/, a package.json
that pins @stellar/stellar-sdk and buffer as peer dependencies, a tsconfig.json
and a README, ready to publish or install from a tarball.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lang != "ts" && lang != "typescript" && lang != "md" && lang != "markdown" &&
				lang != "examples" && lang != "go" && lang != "golang" && lang != "py" && lang != "python" {
				return fmt.Errorf("unsupported --lang %q (supported: ts, go, py, md, examples)", lang)
			}
			if asPackage {
				if lang == "md" || lang == "markdown" || lang == "go" || lang == "golang" ||
					lang == "py" || lang == "python" {
					return fmt.Errorf("--package writes a TypeScript package, so --lang %s cannot be packaged", lang)
				}
				if lang == "examples" {
					return fmt.Errorf("--package writes a TypeScript package, so --lang examples cannot be packaged")
				}
				if strings.TrimSpace(packageName) == "" {
					return fmt.Errorf("--package needs --name: the npm package name to publish")
				}
			} else if pkgVersion != "" {
				return fmt.Errorf("--package-version only applies to --package")
			}
			if withZod && lang != "ts" && lang != "typescript" {
				return fmt.Errorf("--zod generates TypeScript schemas, so it needs --lang ts")
			}

			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				net := network
				if net == "" {
					var err error
					if net, err = a.registry.Network(ctx); err != nil {
						return err
					}
				}

				stored, err := a.registry.Store().GetSpec(ctx, net, args[0], wasmHash)
				if err != nil {
					return err
				}

				src := codegen.Source{
					ContractID: stored.ContractID,
					Network:    stored.Network,
					WasmHash:   stored.WasmHash,
					WithZod:    withZod,
				}

				if asPackage {
					files, err := codegen.Package(stored.Interface, src, codegen.PackageOptions{
						Name:    packageName,
						Version: pkgVersion,
						WithZod: withZod,
					})
					if err != nil {
						return err
					}

					dir := output
					if dir == "" || dir == "-" {
						dir = packageDirName(packageName)
					}
					for _, file := range files {
						path := filepath.Join(dir, filepath.FromSlash(file.Path))
						if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
							return err
						}
						if err := os.WriteFile(path, file.Data, 0o600); err != nil {
							return err
						}
					}
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d files)\n", dir, len(files))
					return nil
				}

				var source string
				switch lang {
				case "md", "markdown":
					source, err = codegen.Markdown(stored.Interface, src)
				case "go", "golang":
					pkg := strings.TrimSpace(packageName)
					if pkg == "" {
						pkg = "contract"
					}
					source, err = codegen.Go(stored.Interface, src, pkg)
				case "py", "python":
					source, err = codegen.Python(stored.Interface, src)
				case "examples":
					var body []byte
					body, err = codegen.Examples(stored.Interface, src)
					source = string(body)
				default:
					source, err = codegen.TypeScript(stored.Interface, src)
				}
				if err != nil {
					return err
				}

				if output == "" || output == "-" {
					_, err = io.WriteString(cmd.OutOrStdout(), source)
					return err
				}
				if err := os.WriteFile(output, []byte(source), 0o600); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", output)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network to read from (default: the network RPC_URL serves)")
	cmd.Flags().StringVar(&wasmHash, "wasm-hash", "", "generate for a specific version instead of the current one")
	cmd.Flags().StringVar(&lang, "lang", "ts", "output format (ts, go, py, md or examples)")
	cmd.Flags().BoolVar(&withZod, "zod", false, "append Zod schemas to the TypeScript output (ts only)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file, or this directory with --package")
	cmd.Flags().BoolVar(&asPackage, "package", false, "write a complete npm package instead of one file")
	cmd.Flags().StringVar(&packageName, "name", "", "npm package name for --package (@scope/name), or Go package name for --lang go")
	cmd.Flags().StringVar(&pkgVersion, "package-version", "", "version for --package (default: derived from the WASM hash)")
	registerContractIDCompletion(cmd)
	registerNetworkCompletion(cmd)
	return cmd
}

// packageDirName is the directory --package writes to when --output is not
// given. A scope is part of the package name, not a directory to create, so
// @acme/zkvote becomes zkvote.
func packageDirName(name string) string {
	if _, base, ok := strings.Cut(name, "/"); ok {
		return base
	}
	return name
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		Long: `Apply pending database migrations.

Run "sorovault migrate status" to see what is applied and what is pending
without applying anything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			databaseURL, err := requireDatabaseURL()
			if err != nil {
				return err
			}

			if err := store.Migrate(databaseURL); err != nil {
				return err
			}

			version, dirty, err := store.MigrationVersion(databaseURL)
			if err != nil {
				return err
			}
			if dirty {
				return fmt.Errorf("database is at version %d but marked dirty; a previous migration failed partway and needs manual repair", version)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "database is at migration version %d\n", version)
			return nil
		},
	}

	cmd.AddCommand(newMigrateStatusCmd(migrateStatusDepsFromEnv))
	return cmd
}

// migrateStatusDeps are the things a migration status run talks to. They are
// injected so the command can be exercised without a database.
type migrateStatusDeps struct {
	// Version reports the migration version the database has applied, and
	// whether the database is marked dirty.
	Version func(ctx context.Context) (version uint, dirty bool, err error)
	// Migrations lists the migrations embedded in the binary, oldest first.
	Migrations func(ctx context.Context) ([]store.Migration, error)
}

// migrateStatusDepsFromEnv builds the status command's dependencies from the
// environment.
func migrateStatusDepsFromEnv() (migrateStatusDeps, error) {
	databaseURL, err := requireDatabaseURL()
	if err != nil {
		return migrateStatusDeps{}, err
	}

	return migrateStatusDeps{
		// golang-migrate is the same source of truth the migrate command
		// applies, so the two cannot disagree about where the database is.
		// Opening a database through it creates the version table if the
		// database has none, which is the one thing a status run may write.
		Version: func(context.Context) (uint, bool, error) {
			return store.MigrationVersion(databaseURL)
		},
		Migrations: func(context.Context) ([]store.Migration, error) {
			return store.EmbeddedMigrationsFor(databaseURL)
		},
	}, nil
}

// migrateStatusReport is what a status run found, in the form the --json
// flag emits.
type migrateStatusReport struct {
	// Current is the migration version the database has applied.
	Current uint `json:"current_version"`
	// Latest is the highest migration version embedded in this binary.
	Latest uint `json:"latest_version"`
	// Dirty says whether the database is marked dirty.
	Dirty bool `json:"dirty"`
	// Pending lists the migrations the database has not applied, oldest
	// first. It is empty rather than null when there are none.
	Pending []migrationStatusItem `json:"pending"`
	// Message says what the report means and what to do about it, so a
	// reader of the JSON is told what a reader of the text report is.
	Message string `json:"message"`
}

// migrationStatusItem is one migration in a status report.
type migrationStatusItem struct {
	// Version is the migration's version number.
	Version uint `json:"version"`
	// Name is the migration's name.
	Name string `json:"name"`
}

// The wording of the report. The text form and the JSON form both come
// from here, so they cannot describe the same state differently.
const (
	// dirtyState says what a dirty database means.
	dirtyState = "the database is marked dirty: a migration failed partway and the schema is in an unknown state"
	// dirtyAdvice says what to do about a dirty database.
	dirtyAdvice = "the pending migrations cannot be applied until the schema is repaired: " +
		"fix the schema by hand, then set the version with the golang-migrate CLI " +
		"(`migrate -path migrations -database \"$DATABASE_URL\" force <version>`) before applying migrations again"
	// upToDate says the database has every migration this binary carries.
	upToDate = "the database is up to date"
)

// statusMessage summarises the report in one line: what state the database
// is in, and what to do about it.
//
// The text report says the same in more than one line where a terminal
// reads better.
func statusMessage(report migrateStatusReport) string {
	switch {
	case report.Dirty:
		return dirtyState + "; " + dirtyAdvice

	case len(report.Pending) > 0:
		return fmt.Sprintf("%d migration(s) pending; run `sorovault migrate` to apply them", len(report.Pending))

	case report.Current > report.Latest:
		return fmt.Sprintf("the database is at version %d, ahead of the migrations in this binary (version %d)",
			report.Current, report.Latest)

	default:
		return upToDate
	}
}

// newMigrateStatusCmd builds the "migrate status" command. Its dependencies
// are injected so it can be exercised without a database.
func newMigrateStatusCmd(depsFor func() (migrateStatusDeps, error)) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show which migrations are applied and which are pending",
		Long: `Report the migration version the database has applied, the latest
version embedded in this binary, and the migrations that have not been
applied yet.

No migration is applied: a status run reports and stops there.

The command exits non-zero when migrations are pending, so it can gate a
deployment.`,
		Args: cobra.NoArgs,
		// The report already says what is wrong, so the error only carries
		// the summary, and main prints that rather than cobra printing it
		// a second time.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, err := depsFor()
			if err != nil {
				return err
			}
			return reportMigrationStatus(cmd.Context(), cmd.OutOrStdout(), deps, asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
}

// reportMigrationStatus works out where the database is, writes the report,
// and fails when the database is not in a state a deployment can start from.
func reportMigrationStatus(ctx context.Context, w io.Writer, deps migrateStatusDeps, asJSON bool) error {
	current, dirty, err := deps.Version(ctx)
	if err != nil {
		return err
	}

	migrations, err := deps.Migrations(ctx)
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		// A binary that embeds no migrations cannot say whether a database
		// is up to date, and saying "up to date" would be a lie.
		return errors.New("this binary embeds no migrations")
	}

	report := migrateStatusReport{
		Current: current,
		Dirty:   dirty,
		Pending: []migrationStatusItem{},
	}
	for _, m := range migrations {
		if m.Version > report.Latest {
			report.Latest = m.Version
		}
		if m.Version > current {
			report.Pending = append(report.Pending, migrationStatusItem{
				Version: m.Version,
				Name:    m.Name,
			})
		}
	}
	report.Message = statusMessage(report)

	if asJSON {
		if err := writeJSON(w, report); err != nil {
			return err
		}
	} else if err := writeMigrationStatus(w, report); err != nil {
		return err
	}

	switch {
	case report.Dirty:
		// A dirty database is the more urgent problem: the schema is in
		// an unknown state, so the pending list cannot be trusted either.
		return errors.New("the database is marked dirty and needs manual repair")

	case len(report.Pending) > 0:
		return errors.New(report.Message)

	default:
		// Nothing is pending, whether the database is at the latest version
		// or ahead of it; the report says which of the two it is.
		return nil
	}
}

// writeMigrationStatus writes the human-readable report.
func writeMigrationStatus(w io.Writer, report migrateStatusReport) error {
	_, _ = fmt.Fprintf(w, "current version: %d\n", report.Current)
	_, _ = fmt.Fprintf(w, "latest version:  %d\n", report.Latest)

	switch {
	case report.Dirty:
		_, _ = fmt.Fprintf(w, "\n%s\n", dirtyState)
		_, _ = fmt.Fprintf(w, "%s\n", dirtyAdvice)
		return nil

	case len(report.Pending) > 0:
		_, _ = fmt.Fprintf(w, "\npending migrations:\n")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  VERSION\tNAME")
		for _, m := range report.Pending {
			_, _ = fmt.Fprintf(tw, "  %d\t%s\n", m.Version, m.Name)
		}
		return tw.Flush()

	case report.Current > report.Latest:
		_, _ = fmt.Fprintf(w, "\n%s\n", report.Message)
		return nil

	default:
		_, _ = fmt.Fprintf(w, "\n%s\n", upToDate)
		return nil
	}
}

// doctorCheck is one thing a doctor run checks.
type doctorCheck struct {
	// Name identifies the check in the report.
	Name string
	// Run performs the check. It returns a line describing what it found,
	// or an error if the check failed.
	Run func(ctx context.Context) (string, error)
}

// doctorResult is the outcome of one check.
type doctorResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Detail describes what the check found.
	Detail string `json:"detail,omitempty"`
	// Error says why the check failed.
	Error string `json:"error,omitempty"`
}

// doctorReport is the outcome of a whole doctor run.
type doctorReport struct {
	OK     bool           `json:"ok"`
	Checks []doctorResult `json:"checks"`
}

// failed counts the checks that did not pass.
func (r doctorReport) failed() int {
	n := 0
	for _, c := range r.Checks {
		if !c.OK {
			n++
		}
	}
	return n
}

// doctorDeps are the things a doctor run talks to. They are injected so the
// command can be exercised without a database or a network.
type doctorDeps struct {
	// Ping checks that the database answers.
	Ping func(ctx context.Context) error
	// Migrations reports the migration version the database has applied,
	// the latest embedded one, and whether the database is marked dirty.
	Migrations func(ctx context.Context) (current, latest uint, dirty bool, err error)
	// Passphrase reads the passphrase the RPC endpoint serves.
	Passphrase func(ctx context.Context) (string, error)
	// ExpectPassphrase is the network the operator configured.
	ExpectPassphrase string
	// Close releases whatever the dependencies hold.
	Close func() error
}

// newDoctorCmd builds the doctor command. Its dependencies are injected so
// the command can be exercised without a database or a network.
func newDoctorCmd(depsFor func() (doctorDeps, error)) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that a deployment is healthy",
		Long: `Check the things that are wrong most often: whether the database
answers, whether its schema is up to date, whether the RPC endpoint is
reachable, and whether it serves the network this instance expects.

Every check is read-only. Nothing is written to the database, no migration
is applied, and nothing is changed on the network.`,
		Args: cobra.NoArgs,
		// The report already says which checks failed, so the error only
		// carries the count and cobra need not print it as well.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, err := depsFor()
			if err != nil {
				return err
			}
			if deps.Close != nil {
				defer func() { _ = deps.Close() }()
			}

			report := runDoctor(cmd.Context(), doctorChecks(deps))

			if asJSON {
				if err := writeJSON(cmd.OutOrStdout(), report); err != nil {
					return err
				}
			} else {
				printDoctor(cmd.OutOrStdout(), report)
			}

			if !report.OK {
				return fmt.Errorf("%d of %d checks failed", report.failed(), len(report.Checks))
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
}

// doctorDepsFromEnv builds the doctor's dependencies from the configuration
// source (the --config file where named, then the environment, as with
// every other command).
func doctorDepsFromEnv() (doctorDeps, error) {
	cfg, err := config.LoadWith(configPath)
	if err != nil {
		return doctorDeps{}, err
	}
	return newDoctorDeps(cfg)
}

// newDoctorDeps builds the doctor's dependencies from the configuration:
// the database at DATABASE_URL and the RPC endpoint at RPC_URL.
//
// The passphrase is read once and remembered, so the reachability and the
// passphrase check cost one round trip between them.
func newDoctorDeps(cfg *config.Config) (doctorDeps, error) {
	fetcher := stellar.NewRPCFetcher(cfg.RPCURL, nil)

	var (
		once       sync.Once
		passphrase string
		passErr    error
	)
	readPassphrase := func(ctx context.Context) (string, error) {
		once.Do(func() { passphrase, passErr = fetcher.Network(ctx) })
		return passphrase, passErr
	}

	return doctorDeps{
		Ping: func(ctx context.Context) error {
			return store.Ping(ctx, cfg.DatabaseURL)
		},
		Migrations: func(ctx context.Context) (uint, uint, bool, error) {
			return store.MigrationStatus(ctx, cfg.DatabaseURL)
		},
		Passphrase:       readPassphrase,
		ExpectPassphrase: cfg.NetworkPassphrase,
		Close:            fetcher.Close,
	}, nil
}

// doctorChecks builds the checks a doctor run performs, in the order they
// are reported.
func doctorChecks(deps doctorDeps) []doctorCheck {
	return []doctorCheck{
		{
			Name: "database",
			Run: func(ctx context.Context) (string, error) {
				if err := deps.Ping(ctx); err != nil {
					return "", err
				}
				return "the database answers", nil
			},
		},
		{
			Name: "migrations",
			Run: func(ctx context.Context) (string, error) {
				current, latest, dirty, err := deps.Migrations(ctx)
				if err != nil {
					return "", err
				}
				if dirty {
					return "", fmt.Errorf("the database is at version %d but marked dirty; a previous migration failed partway and needs manual repair", current)
				}
				if current < latest {
					return "", fmt.Errorf("the database is at version %d, but version %d is available; run `sorovault migrate`", current, latest)
				}
				return fmt.Sprintf("the database is at version %d, the latest is %d", current, latest), nil
			},
		},
		{
			Name: "rpc",
			Run: func(ctx context.Context) (string, error) {
				passphrase, err := deps.Passphrase(ctx)
				if err != nil {
					return "", err
				}
				return "the endpoint is reachable, serving " + stellar.NetworkName(passphrase), nil
			},
		},
		{
			Name: "passphrase",
			Run: func(ctx context.Context) (string, error) {
				passphrase, err := deps.Passphrase(ctx)
				if err != nil {
					return "", err
				}
				if err := registry.CheckPassphrase(passphrase, deps.ExpectPassphrase); err != nil {
					return "", err
				}
				return "the endpoint serves the configured network", nil
			},
		},
	}
}

// runDoctor runs every check and collects the results. A check that fails
// does not stop the ones after it: the point of a doctor run is to say
// everything that is wrong, not just the first thing.
func runDoctor(ctx context.Context, checks []doctorCheck) doctorReport {
	report := doctorReport{OK: true, Checks: make([]doctorResult, 0, len(checks))}

	for _, check := range checks {
		detail, err := check.Run(ctx)
		result := doctorResult{Name: check.Name, OK: err == nil, Detail: detail}
		if err != nil {
			result.Error = err.Error()
			report.OK = false
		}
		report.Checks = append(report.Checks, result)
	}

	return report
}

// printDoctor writes the report as one line per check.
func printDoctor(w io.Writer, report doctorReport) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range report.Checks {
		status, detail := "PASS", c.Detail
		if !c.OK {
			status, detail = "FAIL", c.Error
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", status, c.Name, detail)
	}
	_ = tw.Flush()
}

// printRegisterResult writes the human-readable summary of an add.
func printRegisterResult(w io.Writer, r *registry.Result) {
	verb := "already current"
	switch {
	case r.Missing:
		verb = "missing from the network"
	case r.Created:
		verb = "registered"
	case r.Changed:
		verb = "upgraded"
	}

	_, _ = fmt.Fprintf(w, "%s %s on %s\n", r.Contract.ContractID, verb, r.Contract.Network)
	_, _ = fmt.Fprintf(w, "  wasm hash  %s\n", r.Contract.CurrentWasmHash)
	printOrigin(w, r.Contract)

	if r.Interface != nil {
		_, _ = fmt.Fprintf(w, "  functions  %d\n", len(r.Interface.Functions))
		if n := len(r.Interface.Types.Structs) + len(r.Interface.Types.Unions) +
			len(r.Interface.Types.Enums) + len(r.Interface.Types.ErrorEnums); n > 0 {
			_, _ = fmt.Fprintf(w, "  types      %d\n", n)
		}
		if n := len(r.Interface.Events); n > 0 {
			_, _ = fmt.Fprintf(w, "  events     %d\n", n)
		}
	}
}

// printInterface writes a contract's interface as readable signatures.
func printInterface(w io.Writer, s *store.Spec) {
	_, _ = fmt.Fprintf(w, "%s  (%s)\n", s.ContractID, s.Network)
	_, _ = fmt.Fprintf(w, "wasm %s\n\n", s.WasmHash)

	if len(s.Interface.Functions) == 0 {
		_, _ = fmt.Fprintln(w, "no functions")
	}
	for _, fn := range s.Interface.Functions {
		_, _ = fmt.Fprintf(w, "  %s\n", fn.Signature())
	}

	t := s.Interface.Types
	if n := len(t.Structs) + len(t.Unions) + len(t.Enums) + len(t.ErrorEnums); n > 0 {
		_, _ = fmt.Fprintln(w, "\ntypes:")
		for _, x := range t.Structs {
			_, _ = fmt.Fprintf(w, "  struct %s (%d fields)\n", x.Name, len(x.Fields))
		}
		for _, x := range t.Unions {
			_, _ = fmt.Fprintf(w, "  enum   %s (%d cases)\n", x.Name, len(x.Cases))
		}
		for _, x := range t.Enums {
			_, _ = fmt.Fprintf(w, "  enum   %s (%d cases, u32)\n", x.Name, len(x.Cases))
		}
		for _, x := range t.ErrorEnums {
			_, _ = fmt.Fprintf(w, "  error  %s (%d cases)\n", x.Name, len(x.Cases))
		}
	}

	if len(s.Interface.Events) > 0 {
		_, _ = fmt.Fprintln(w, "\nevents:")
		for _, e := range s.Interface.Events {
			_, _ = fmt.Fprintf(w, "  %s (%d params)\n", e.Name, len(e.Params))
		}
	}
}

// versionsDocument is the JSON shape of the versions command. It matches
// what GET /api/contracts/{id}/versions serves, so the two are
// interchangeable in a pipeline.
type versionsDocument struct {
	ContractID string              `json:"contract_id"`
	Network    string              `json:"network"`
	Versions   []store.SpecVersion `json:"versions"`
}

// printVersions writes one row per stored interface, newest first.
func printVersions(w io.Writer, versions []store.SpecVersion) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WASM HASH\tDECODED\tCURRENT")
	for _, v := range versions {
		current := ""
		if v.Current {
			current = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
			v.WasmHash, v.DecodedAt.UTC().Format("2006-01-02 15:04"), current)
	}
	_ = tw.Flush()
}

// printFunction writes one function's signature in detail.
func printFunction(w io.Writer, fn model.Function) {
	_, _ = fmt.Fprintf(w, "%s\n", fn.Signature())

	if fn.Doc != "" {
		for _, line := range strings.Split(strings.TrimRight(fn.Doc, "\n"), "\n") {
			_, _ = fmt.Fprintf(w, "  // %s\n", line)
		}
	}

	if len(fn.Inputs) > 0 {
		_, _ = fmt.Fprintln(w, "\ninputs:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, in := range fn.Inputs {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\n", in.Name, in.Type.Display)
		}
		_ = tw.Flush()
	}

	_, _ = fmt.Fprint(w, "\nreturns: ")
	if len(fn.Outputs) == 0 {
		_, _ = fmt.Fprintln(w, "()")
		return
	}
	_, _ = fmt.Fprintln(w, fn.Outputs[0].Display)
}

// writeJSON emits an indented JSON document followed by a newline.
//
// HTML escaping is off: this JSON is read by other programs, not embedded
// in a page, and escaping would turn a placeholder like <version> into
// \u003cversion\u003e, which is harder to read in a terminal and no safer
// once parsed.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
