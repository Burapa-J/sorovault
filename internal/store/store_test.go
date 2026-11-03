package store_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/store"
)

// TestDatabaseURLEnv names the variable that points the suite at a real
// Postgres. When it is unset the Postgres cases skip, so `go test ./...`
// needs no database and no network.
const TestDatabaseURLEnv = "TEST_DATABASE_URL"

// factory builds a fresh, empty store for one subtest.
type factory func(t *testing.T) store.Store

// TestStore runs one conformance suite against every implementation, so the
// in-memory store used by other packages' tests cannot drift from the
// Postgres one it stands in for.
func TestStore(t *testing.T) {
	t.Parallel()

	impls := map[string]factory{
		"memory": func(t *testing.T) store.Store { return store.NewMemory() },
		"sqlite": newSQLiteStore,
	}
	if url := os.Getenv(TestDatabaseURLEnv); url != "" {
		impls["postgres"] = postgresFactory(t, url)
	}

	for name, newStore := range impls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runStoreSuite(t, newStore)
		})
	}
}

// TestPostgresIsExercised fails if a CI run silently skipped the Postgres
// half of the suite, which would let the two implementations diverge unseen.
func TestPostgresIsExercised(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("only enforced in CI")
	}
	require.NotEmpty(t, os.Getenv(TestDatabaseURLEnv),
		TestDatabaseURLEnv+" must be set in CI so the Postgres store is covered")
}

// postgresFactory prepares a shared pool and hands each subtest a store over
// a schema of its own, so cases stay isolated while sharing one connection.
func postgresFactory(t *testing.T, url string) factory {
	t.Helper()

	require.NoError(t, store.Migrate(url), "migrating test database")

	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, pool.Ping(context.Background()))

	return func(t *testing.T) store.Store {
		// Truncating rather than recreating keeps setup cheap; the cascade
		// clears specs along with contracts. Subscriptions stand alone, so
		// they are named too.
		_, err := pool.Exec(context.Background(), "TRUNCATE contracts, subscriptions CASCADE")
		require.NoError(t, err)

		return store.NewPostgresFromPool(pool)
	}
}

// iface builds a small interface with a single named function.
func iface(fnName string) *model.Interface {
	return &model.Interface{
		ABIVersion: model.ABIVersion,
		Functions: []model.Function{{
			Name:    fnName,
			Inputs:  []model.Param{{Name: "who", Type: model.Scalar(model.KindAddress)}},
			Outputs: []model.Type{model.Scalar(model.KindU32)},
		}},
		Types:  model.Types{Structs: []model.Struct{}, Unions: []model.Union{}, Enums: []model.Enum{}, ErrorEnums: []model.ErrorEnum{}},
		Events: []model.Event{},
	}
}

func standardInterface() *model.Interface {
	functions := make([]model.Function, 0, len(spec.SEP41.Functions))
	for _, required := range spec.SEP41.Functions {
		inputs := make([]model.Param, 0, len(required.Inputs))
		for i, typ := range required.Inputs {
			inputs = append(inputs, model.Param{Name: string(rune('a' + i)), Type: typ})
		}
		functions = append(functions, model.Function{Name: required.Name, Inputs: inputs, Outputs: append([]model.Type(nil), required.Outputs...)})
	}
	return &model.Interface{ABIVersion: model.ABIVersion, Functions: functions}
}

func contract(id, network, hash string) store.Contract {
	return store.Contract{ContractID: id, Network: network, CurrentWasmHash: hash}
}

// unique returns the values with duplicates collapsed, so a walk's
// completeness and the absence of repeats can be one assertion each.
func unique(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// pageIDs lists a page's contract IDs in arriving order, for asserting the
// listing's rank.
func pageIDs(p *store.ContractPage) []string {
	out := make([]string, 0, len(p.Contracts))
	for _, c := range p.Contracts {
		out = append(out, c.ContractID)
	}
	return out
}

const (
	idA = "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"
	idB = "CDZZZPYQALFRBQAZD5KCW2SZK4ZF455PJVD7H53AGHXTWAJO4BYVPP2V"
)

func runStoreSuite(t *testing.T, newStore factory) {
	t.Run("subscriptions round-trip by secret", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		empty, err := s.ListSubscriptions(ctx)
		require.NoError(t, err)
		assert.NotNil(t, empty, "an empty listing is an empty list, not null")
		assert.Empty(t, empty)

		secret, err := s.Subscribe(ctx, "testnet", []string{idA, idB}, "https://hooks.example.test")
		require.NoError(t, err)
		other, err := s.Subscribe(ctx, "testnet", []string{idA}, "https://other.example.test")
		require.NoError(t, err)
		assert.NotEqual(t, secret, other, "every subscription gets its own secret")

		sub, err := s.GetSubscription(ctx, secret)
		require.NoError(t, err)
		assert.Equal(t, "testnet", sub.Network)
		assert.Equal(t, []string{idA, idB}, sub.ContractIDs)
		assert.Equal(t, "https://hooks.example.test", sub.URL)
		assert.True(t, sub.Active)

		items, err := s.ListSubscriptions(ctx)
		require.NoError(t, err)
		require.Len(t, items, 2)
		counts := map[string]int{}
		for _, item := range items {
			counts[item.ID] = item.Contracts
		}
		assert.Equal(t, map[string]int{secret: 2, other: 1}, counts)

		require.NoError(t, s.MarkSubscriptionInactive(ctx, secret))
		sub, err = s.GetSubscription(ctx, secret)
		require.NoError(t, err)
		assert.False(t, sub.Active)

		require.NoError(t, s.Unsubscribe(ctx, secret))
		_, err = s.GetSubscription(ctx, secret)
		require.ErrorIs(t, err, store.ErrNotFound)
		require.ErrorIs(t, s.Unsubscribe(ctx, secret), store.ErrNotFound)
		require.ErrorIs(t, s.MarkSubscriptionInactive(ctx, secret), store.ErrNotFound)
	})

	t.Run("filters by exact implemented standard", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "token-hash"), standardInterface()))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "plain-hash"), iface("transfer")))

		page, err := s.ListContracts(ctx, store.ListFilter{Implements: []string{"sep41"}})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		require.Len(t, page.Contracts, 1)
		assert.Equal(t, idA, page.Contracts[0].ContractID)
		assert.Equal(t, []string{"sep41"}, page.Contracts[0].Implements)
	})
	t.Run("catalogued wasm does not require a contract", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.SaveWasmSpec(ctx, "testnet", "hash1", iface("proposed")))
		got, err := s.GetWasmSpec(ctx, "testnet", "hash1")
		require.NoError(t, err)
		assert.Equal(t, "proposed", got.Interface.Functions[0].Name)
		_, err = s.GetContract(ctx, "testnet", idA)
		assert.ErrorIs(t, err, store.ErrNotFound, "cataloguing code must not create a contract row")
	})

	t.Run("save and read back", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))

		got, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Equal(t, idA, got.ContractID)
		assert.Equal(t, "hash1", got.CurrentWasmHash)
		assert.False(t, got.FirstSeen.IsZero())

		spec, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "hash1", spec.WasmHash)
		require.Len(t, spec.Interface.Functions, 1)
		assert.Equal(t, "balance", spec.Interface.Functions[0].Name)
		assert.Equal(t, "Address", spec.Interface.Functions[0].Inputs[0].Type.Display)

		require.NoError(t, s.UpdateSource(ctx, "testnet", idA, "https://github.com/example/repo", "0123456789abcdef0123456789abcdef01234567"))
		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Equal(t, "https://github.com/example/repo", got.SourceURL)
		assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", got.SourceCommit)

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))
		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Equal(t, "https://github.com/example/repo", got.SourceURL, "refresh must preserve source metadata")
	})

	t.Run("a deprecation mark survives refresh and clears cleanly", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "hash2"), iface("balance")))

		require.NoError(t, s.SetDeprecation(ctx, "testnet", idA, idB, "Move voting to the replacement."))
		got, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		require.NotNil(t, got.DeprecatedSince, "the mark carries the moment it was set")
		assert.Equal(t, idB, got.Successor)
		assert.Equal(t, "Move voting to the replacement.", got.DeprecationNote)

		// A refresh does not revisit the mark: a contract being live on
		// the network says nothing about whether it should still be used.
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash3"), iface("balance-v2")))
		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.NotNil(t, got.DeprecatedSince, "a refresh does not un-deprecate a contract")
		assert.Equal(t, idB, got.Successor)

		// Without a successor or note, only the timestamp is set.
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "hash2"), iface("balance")))
		require.NoError(t, s.ClearDeprecation(ctx, "testnet", idB))
		require.NoError(t, s.SetDeprecation(ctx, "testnet", idB, "", ""))
		got, err = s.GetContract(ctx, "testnet", idB)
		require.NoError(t, err)
		assert.NotNil(t, got.DeprecatedSince)
		assert.Empty(t, got.Successor)
		assert.Empty(t, got.DeprecationNote)

		// Clearing drops the whole mark.
		require.NoError(t, s.ClearDeprecation(ctx, "testnet", idA))
		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Nil(t, got.DeprecatedSince)
		assert.Empty(t, got.Successor)
		assert.Empty(t, got.DeprecationNote)

		// Unknown contracts are not silently marked.
		err = s.SetDeprecation(ctx, "testnet", "CDZZNOPE", "", "")
		assert.ErrorIs(t, err, store.ErrNotFound)
		err = s.ClearDeprecation(ctx, "testnet", "CDZZNOPE")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("a deprecation mark rides listings, exports and imports", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "hash2"), iface("balance")))
		require.NoError(t, s.SetDeprecation(ctx, "testnet", idB, idA, "Move over."))

		page, err := s.ListContracts(ctx, store.ListFilter{Network: "testnet"})
		require.NoError(t, err)
		require.Equal(t, 2, page.Total)
		for _, c := range page.Contracts {
			if c.ContractID != idB {
				assert.Nil(t, c.DeprecatedSince, idA+" is still in active use")
				continue
			}
			assert.NotNil(t, c.DeprecatedSince, "the listing carries the mark")
			assert.Equal(t, idA, c.Successor)
			assert.Equal(t, "Move over.", c.DeprecationNote)
		}

		before, err := s.Export(ctx)
		require.NoError(t, err)
		for _, cs := range before.Contracts {
			if cs.Contract.ContractID == idB {
				require.NotNil(t, cs.Contract.DeprecatedSince, "the snapshot carries the mark")
				assert.Equal(t, idA, cs.Contract.Successor)
				assert.Equal(t, "Move over.", cs.Contract.DeprecationNote)
			}
		}

		// A fresh store restores the whole mark.
		other := newStore(t)
		require.NoError(t, other.Import(ctx, before, false))
		got, err := other.GetContract(ctx, "testnet", idB)
		require.NoError(t, err)
		require.NotNil(t, got.DeprecatedSince)
		assert.Equal(t, idA, got.Successor)
		assert.Equal(t, "Move over.", got.DeprecationNote)
	})

	t.Run("missing rows report ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		_, err := s.GetContract(ctx, "testnet", idA)
		assert.ErrorIs(t, err, store.ErrNotFound)

		_, err = s.GetSpec(ctx, "testnet", idA, "")
		assert.ErrorIs(t, err, store.ErrNotFound)

		_, err = s.ListVersions(ctx, "testnet", idA)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("resaving the same hash does not add a version", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))

		versions, err := s.ListVersions(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Len(t, versions, 1, "re-registering an unchanged contract must be idempotent")
	})

	t.Run("upgrading keeps the prior version", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("old_fn")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash2"), iface("new_fn")))

		current, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "hash2", current.WasmHash)
		assert.Equal(t, "new_fn", current.Interface.Functions[0].Name)

		// The whole point of versioning: the superseded interface is still
		// retrievable by its hash.
		old, err := s.GetSpec(ctx, "testnet", idA, "hash1")
		require.NoError(t, err)
		assert.Equal(t, "old_fn", old.Interface.Functions[0].Name)

		versions, err := s.ListVersions(ctx, "testnet", idA)
		require.NoError(t, err)
		require.Len(t, versions, 2)

		var currentCount int
		for _, v := range versions {
			if v.Current {
				currentCount++
				assert.Equal(t, "hash2", v.WasmHash)
			}
		}
		assert.Equal(t, 1, currentCount, "exactly one version is current")
	})

	t.Run("first_seen survives a refresh", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("f")))
		first, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)

		time.Sleep(5 * time.Millisecond)
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash2"), iface("f")))

		after, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)

		assert.WithinDuration(t, first.FirstSeen, after.FirstSeen, time.Millisecond,
			"first_seen records when the contract was first registered, not last refreshed")
		assert.False(t, after.LastRefreshed.Before(first.LastRefreshed))
	})

	t.Run("unknown version hash is not found", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("f")))

		_, err := s.GetSpec(ctx, "testnet", idA, "nonexistent")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("networks are separate namespaces", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash-t"), iface("on_testnet")))
		require.NoError(t, s.Save(ctx, contract(idA, "public", "hash-p"), iface("on_public")))

		testnet, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "on_testnet", testnet.Interface.Functions[0].Name)

		public, err := s.GetSpec(ctx, "public", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "on_public", public.Interface.Functions[0].Name)

		page, err := s.ListContracts(ctx, store.ListFilter{Network: "public"})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total)
	})

	t.Run("network listing is distinct and sorted", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idB, "public", "h2"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h3"), iface("f")))

		networks, err := s.ListNetworks(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"public", "testnet"}, networks)
	})

	t.Run("listing filters and pages", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("f")))

		all, err := s.ListContracts(ctx, store.ListFilter{})
		require.NoError(t, err)
		assert.Equal(t, 2, all.Total)
		assert.Len(t, all.Contracts, 2)

		// Both fixture IDs share a "CDZZ" prefix, so the query has to be
		// long enough to discriminate.
		match, err := s.ListContracts(ctx, store.ListFilter{Query: "CDZZXV"})
		require.NoError(t, err)
		require.Equal(t, 1, match.Total)
		assert.Equal(t, idA, match.Contracts[0].ContractID)

		none, err := s.ListContracts(ctx, store.ListFilter{Query: "no-such-contract"})
		require.NoError(t, err)
		assert.Zero(t, none.Total)
		assert.Empty(t, none.Contracts, "an empty result must be [] rather than nil")

		firstPage, err := s.ListContracts(ctx, store.ListFilter{Limit: 1})
		require.NoError(t, err)
		assert.Len(t, firstPage.Contracts, 1)
		assert.Equal(t, 2, firstPage.Total, "total counts matches, not the page")

		secondPage, err := s.ListContracts(ctx, store.ListFilter{Limit: 1, Offset: 1})
		require.NoError(t, err)
		require.Len(t, secondPage.Contracts, 1)
		assert.NotEqual(t, firstPage.Contracts[0].ContractID, secondPage.Contracts[0].ContractID)

		past, err := s.ListContracts(ctx, store.ListFilter{Limit: 10, Offset: 99})
		require.NoError(t, err)
		assert.Empty(t, past.Contracts)
		assert.Equal(t, 2, past.Total, "paging past the end still reports the true total")
	})

	t.Run("cursor pagination walks every contract exactly once", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		idC := idA[:len(idA)-1] + "X"
		for _, id := range []string{idA, idB, idC} {
			require.NoError(t, s.Save(ctx, contract(id, "testnet", "h1"), iface("f")))
		}

		// One contract per page, so every page boundary is exercised.
		var seen []string
		cursor := ""
		for {
			page, err := s.ListContracts(ctx, store.ListFilter{Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			for _, c := range page.Contracts {
				seen = append(seen, c.Network+"/"+c.ContractID)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		require.Len(t, seen, 3, "the walk reaches every contract")
		assert.Len(t, unique(seen), 3, "no contract repeats across page boundaries")

		// And the whole listing matches walking by offset.
		offsetList, err := s.ListContracts(ctx, store.ListFilter{Limit: store.MaxLimit})
		require.NoError(t, err)
		var listed []string
		for _, c := range offsetList.Contracts {
			listed = append(listed, c.Network+"/"+c.ContractID)
		}
		assert.Equal(t, listed, seen, "a cursor walk covers the same rows in the same order")
	})

	t.Run("a contract added while a client pages is never repeated", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		idC := idA[:len(idA)-1] + "X"
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h1"), iface("f")))

		firstPage, err := s.ListContracts(ctx, store.ListFilter{Limit: 1})
		require.NoError(t, err)
		require.NotEmpty(t, firstPage.Next, "two contracts over a limit of one leave a next page")

		// The arrival the offset walk trips over: the fresh row shifts
		// every page after it down by one, and the row at the boundary is
		// returned twice. A cursor walk has no positions to shift, so the
		// rows must not double up no matter where the newcomer sorts.
		require.NoError(t, s.Save(ctx, contract(idC, "testnet", "h1"), iface("f")))

		walked := []string{firstPage.Contracts[0].Network + "/" + firstPage.Contracts[0].ContractID}
		cursor := firstPage.Next
		for {
			page, err := s.ListContracts(ctx, store.ListFilter{Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			for _, c := range page.Contracts {
				walked = append(walked, c.Network+"/"+c.ContractID)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}

		// Every contract the walk started with is still reached — where it
		// sits on the page or after it — and none of them twice. Whether
		// the newcomer itself is inside this walk depends on where its
		// refresh time sorts it against the cursor, so its turn is
		// asserted on a walk that starts after it exists.
		for _, want := range []string{"testnet/" + idA, "testnet/" + idB} {
			assert.Contains(t, walked, want, "the walk that started before the arrival still reaches it")
		}
		assert.Len(t, unique(walked), len(walked), "nothing repeats")

		// And a walk that starts after the arrival covers everything
		// — the newcomer included — exactly once.
		var settled []string
		cursor = ""
		for {
			page, err := s.ListContracts(ctx, store.ListFilter{Limit: 2, Cursor: cursor})
			require.NoError(t, err)
			for _, c := range page.Contracts {
				settled = append(settled, c.Network+"/"+c.ContractID)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		require.Len(t, settled, 3, "the newcomer is in the walk that started after it exists")
		assert.Len(t, unique(settled), 3, "and exactly once")
	})

	t.Run("the same contract on two networks pages without a gap", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		// One contract ID registered on two networks refreshes in the same
		// instant: the listing's network tiebreak keeps a cursor from
		// seeing one of the two twice or never.
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idA, "mainnet", "h1"), iface("f")))

		var seen []string
		cursor := ""
		for {
			page, err := s.ListContracts(ctx, store.ListFilter{Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			for _, c := range page.Contracts {
				seen = append(seen, c.Network)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		assert.ElementsMatch(t, []string{"testnet", "mainnet"}, seen,
			"both rows of the ID are reached exactly once")
	})

	t.Run("a malformed cursor is rejected", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		_, err := s.ListContracts(ctx, store.ListFilter{Cursor: "not-a-cursor"})
		assert.ErrorIs(t, err, store.ErrMalformedCursor)

		// Valid base64 around the wrong document is the same failure.
		blank := base64.RawURLEncoding.EncodeToString([]byte(`{"x":1}`))
		_, err = s.ListContracts(ctx, store.ListFilter{Cursor: blank})
		assert.ErrorIs(t, err, store.ErrMalformedCursor)
	})

	t.Run("a cursor and an offset are exclusive", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		_, err := s.ListContracts(ctx, store.ListFilter{Limit: 1, Offset: 1, Cursor: "not-a-cursor"})
		require.Error(t, err)
		assert.NotErrorIs(t, err, store.ErrMalformedCursor,
			"the contradiction is reported before the cursor is even read")
	})

	t.Run("search is case-insensitive", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "cdzzxv"})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total)
	})

	t.Run("search matches interface symbols", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		rich := iface("get_proposal")
		rich.Functions = append(rich.Functions, model.Function{Name: "vote"})
		rich.Types.Structs = []model.Struct{{Name: "Proposal", Fields: []model.StructField{}}}
		rich.Events = []model.Event{{Name: "Voted", Params: []model.EventParam{}}}
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), rich))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("transfer")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "PROPOSAL"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, idA, page.Contracts[0].ContractID)
		assert.Equal(t, []string{"Proposal", "get_proposal"}, page.Contracts[0].Matches)

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "vote"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, []string{"Voted", "vote"}, page.Contracts[0].Matches,
			"events are searchable too")
	})

	t.Run("search matches doc comments when no name does", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		mint := iface("mint")
		mint.Functions[0].Doc = "Mints new tokens to a recipient and emits a Minted event."
		burn := iface("burn")
		burn.Functions[0].Doc = "Burns tokens from the caller."
		receipt := model.Struct{
			Name: "BurnReceipt", Fields: []model.StructField{},
			Doc: "Records who initiated the burn.",
		}
		burn.Types.Structs = append(burn.Types.Structs, receipt)
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), mint))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), burn))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "recipient"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total, "a word only a function's doc carries is searchable")
		assert.Equal(t, idA, page.Contracts[0].ContractID)
		assert.Empty(t, page.Contracts[0].Matches, "a doc match is not a symbol match")

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "initiated"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total, "a word only a type's doc carries is searchable")
		assert.Equal(t, idB, page.Contracts[0].ContractID)

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "recipient tokens"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total, "every word of the query must be found in the docs")

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "recipient nowhere"})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "one missing word rules the doc out")

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "%%%!!!"})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "a query with nothing word-like in it matches no doc")

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "min"})
		require.Equal(t, 1, page.Total, "a query both a symbol and a doc carry ranks as a name hit")
		require.NoError(t, err)
		assert.Equal(t, []string{"mint"}, page.Contracts[0].Matches,
			"a doc hit that also names a symbol still reports the symbol match")
	})

	t.Run("a name match ranks ahead of a doc match", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		// The doc-only contract is saved second, so it refreshed last and
		// would sort in front on refresh time alone; the listing ranks the
		// symbol hit ahead of it regardless.
		documented := iface("send")
		documented.Functions[0].Doc = "Sends value across chains via a swap."
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("swap")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), documented))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "swap"})
		require.NoError(t, err)
		require.Equal(t, 2, page.Total)
		assert.Equal(t, []string{idB, idA}, pageIDs(page),
			"the symbol hit is listed first even though the doc hit refreshed last")
	})

	t.Run("a cursor walk with a query crosses the rank boundary", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		documented := iface("send")
		documented.Functions[0].Doc = "Sends value across chains via a swap."
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("swap")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), documented))

		var seen []string
		cursor := ""
		for {
			page, err := s.ListContracts(ctx, store.ListFilter{Query: "swap", Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			for _, c := range page.Contracts {
				seen = append(seen, c.ContractID)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		require.Len(t, unique(seen), 2, "the walk reaches both tiers of the listing")
		assert.Equal(t, idB, seen[0], "the name tier is walked first")
		assert.Equal(t, idA, seen[1], "the doc tier is walked second")
	})

	t.Run("doc search uses only the current interface", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		oldVersion := iface("mint")
		oldVersion.Functions[0].Doc = "Mints tokens at a discount while the sale is on."
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), oldVersion))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "discount"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)

		upgraded := iface("mint")
		upgraded.Functions[0].Doc = "Mints tokens to a recipient."
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h2"), upgraded))

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "discount"})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "a doc removed by an upgrade must stop matching")
	})

	t.Run("search uses only the current interface", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("legacy_mint")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h2"), iface("mint")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "legacy"})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "a function removed by an upgrade must stop matching")

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "mint"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, []string{"mint"}, page.Contracts[0].Matches)
	})

	t.Run("an id match carries no symbol matches", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: idA[:10]})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Empty(t, page.Contracts[0].Matches)

		page, err = s.ListContracts(ctx, store.ListFilter{})
		require.NoError(t, err)
		assert.Empty(t, page.Contracts[0].Matches, "an unfiltered listing reports no matches")
	})

	t.Run("symbol search total is stable past the last page", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("swap")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("swap_exact")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "swap", Offset: 10})
		require.NoError(t, err)
		assert.Empty(t, page.Contracts)
		assert.Equal(t, 2, page.Total)
	})

	t.Run("wildcards in a query are literal", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		// "%" must match a literal percent sign, not every row.
		for _, q := range []string{"%", "_", "%%"} {
			page, err := s.ListContracts(ctx, store.ListFilter{Query: q})
			require.NoError(t, err)
			assert.Zero(t, page.Total, "query %q must be treated as literal text", q)
		}
	})

	t.Run("exact function names are matched exactly", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		rich := iface("transfer")
		rich.Functions = append(rich.Functions, model.Function{Name: "transfer_from"})
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), rich))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("balance")))

		page, err := s.ListContracts(ctx, store.ListFilter{Functions: []string{"transfer"}})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, idA, page.Contracts[0].ContractID,
			"transfer_from is not transfer")

		page, err = s.ListContracts(ctx, store.ListFilter{Functions: []string{"transfer_from"}})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, idA, page.Contracts[0].ContractID)

		page, err = s.ListContracts(ctx, store.ListFilter{Functions: []string{"Transfer"}})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "the match is exact, and so is the case")

		// Every requested function has to be there.
		page, err = s.ListContracts(ctx, store.ListFilter{Functions: []string{"transfer", "balance"}})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "no contract declares both functions")

		page, err = s.ListContracts(ctx, store.ListFilter{Functions: []string{"transfer", "transfer_from"}})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, idA, page.Contracts[0].ContractID)
	})

	t.Run("exact type names are matched exactly", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		rich := iface("f")
		rich.Types.Structs = []model.Struct{{Name: "Balance", Fields: []model.StructField{}}}
		rich.Types.Enums = []model.Enum{{Name: "State", Cases: []model.EnumCase{}}}
		rich.Types.ErrorEnums = []model.ErrorEnum{{Name: "Failed", Cases: []model.EnumCase{}}}
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), rich))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("g")))

		for _, name := range []string{"Balance", "State", "Failed"} {
			page, err := s.ListContracts(ctx, store.ListFilter{Types: []string{name}})
			require.NoError(t, err)
			require.Equal(t, 1, page.Total, "type %q", name)
			assert.Equal(t, idA, page.Contracts[0].ContractID)
		}

		page, err := s.ListContracts(ctx, store.ListFilter{Types: []string{"Bal"}})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "a type name is matched exactly, not as a prefix")

		page, err = s.ListContracts(ctx, store.ListFilter{Types: []string{"Balance", "State"}})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, idA, page.Contracts[0].ContractID)
	})

	t.Run("exact filters combine with the query and the network", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("transfer")))
		require.NoError(t, s.Save(ctx, contract(idB, "public", "h2"), iface("balance")))

		page, err := s.ListContracts(ctx, store.ListFilter{
			Functions: []string{"balance"},
			Network:   "public",
		})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, idB, page.Contracts[0].ContractID)

		page, err = s.ListContracts(ctx, store.ListFilter{
			Functions: []string{"balance"},
			Query:     "CDZZZPY",
		})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, idB, page.Contracts[0].ContractID)

		// The query matches one contract and the function filter another,
		// so nothing matches both.
		page, err = s.ListContracts(ctx, store.ListFilter{
			Functions: []string{"balance"},
			Query:     "CDZZXV",
		})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "the query and the function filter must both match")
	})

	t.Run("a contract that left the network is marked missing", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		since := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		require.NoError(t, s.MarkMissing(ctx, "testnet", idA, since))

		got, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		require.NotNil(t, got.MissingSince)
		assert.Equal(t, since, got.MissingSince.UTC())

		// A second sighting must not move the mark: it says when the
		// contract went away, not when it was last looked for.
		require.NoError(t, s.MarkMissing(ctx, "testnet", idA, since.Add(time.Hour)))
		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		require.NotNil(t, got.MissingSince)
		assert.Equal(t, since, got.MissingSince.UTC())

		// Saving the contract again means it was live, so the mark goes.
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))
		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Nil(t, got.MissingSince)
	})

	t.Run("marking an unknown contract is not found", func(t *testing.T) {
		s := newStore(t)

		err := s.MarkMissing(context.Background(), "testnet", idA, time.Now())
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("query is trimmed", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "  CDZZXV  "})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total)
	})

	t.Run("limit is clamped", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		page, err := s.ListContracts(ctx, store.ListFilter{Limit: 10_000})
		require.NoError(t, err)
		assert.Len(t, page.Contracts, 1, "an oversized limit must be clamped, not rejected")
	})

	t.Run("origin is recorded and survives a save", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("a")))

		got, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Empty(t, got.Creator, "an origin nobody recorded is unknown")
		assert.Zero(t, got.CreatedLedger)

		require.NoError(t, s.SetOrigin(ctx, "testnet", idA, "GDKYNFLCSIELXMVTS6F76CZBEGDPNZ5OSCXRBDP3R3GPPGMJ4Y4CJHCH", 5342891))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash2"), iface("b")))

		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Equal(t, "GDKYNFLCSIELXMVTS6F76CZBEGDPNZ5OSCXRBDP3R3GPPGMJ4Y4CJHCH", got.Creator)
		assert.Equal(t, uint32(5342891), got.CreatedLedger, "an upgrade must not forget who deployed the contract")

		assert.ErrorIs(t, s.SetOrigin(ctx, "testnet", idB, "G", 1), store.ErrNotFound)
	})

	t.Run("verification is recorded, listed and survives a save", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("a")))
		assert.False(t, mustContract(t, s, idA).Verified())

		at := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
		require.NoError(t, s.SetVerified(ctx, "testnet", idA, "ZK Vote", "https://github.com/example/zkvote",
			"0123456789abcdef0123456789abcdef01234567",
			store.Verification{Publisher: "GPUBLISHER", VerifiedAt: at, WasmHash: "hash1"}))

		got := mustContract(t, s, idA)
		assert.True(t, got.Verified())
		assert.Equal(t, "GPUBLISHER", got.VerifiedBy)
		assert.Equal(t, "hash1", got.VerifiedWasmHash)
		assert.True(t, at.Equal(*got.VerifiedAt))
		assert.Equal(t, "ZK Vote", got.Name)
		assert.Equal(t, "https://github.com/example/zkvote", got.SourceURL)

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash2"), iface("b")))
		got = mustContract(t, s, idA)
		assert.True(t, got.Verified(), "an upgrade keeps the verification, pinned to the version it was made for")
		assert.Equal(t, "hash1", got.VerifiedWasmHash)
		assert.Equal(t, "hash2", got.CurrentWasmHash)

		page, err := s.ListContracts(ctx, store.ListFilter{Network: "testnet"})
		require.NoError(t, err)
		require.Len(t, page.Contracts, 1)
		assert.True(t, page.Contracts[0].Verified())

		assert.ErrorIs(t, s.SetVerified(ctx, "testnet", idB, "n", "https://x.example", "", store.Verification{Publisher: "G", VerifiedAt: at}), store.ErrNotFound)
	})

	t.Run("origin and verification round trip through a snapshot", func(t *testing.T) {
		src := newStore(t)
		ctx := context.Background()
		require.NoError(t, src.Save(ctx, contract(idA, "testnet", "hash1"), iface("a")))
		require.NoError(t, src.SetOrigin(ctx, "testnet", idA, "GCREATOR", 77))
		require.NoError(t, src.SetVerified(ctx, "testnet", idA, "N", "https://x.example", "",
			store.Verification{Publisher: "GCREATOR", VerifiedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), WasmHash: "hash1"}))

		snapshot, err := src.Export(ctx)
		require.NoError(t, err)

		dst := newStore(t)
		require.NoError(t, dst.Import(ctx, snapshot, false))
		got := mustContract(t, dst, idA)
		assert.Equal(t, "GCREATOR", got.Creator)
		assert.Equal(t, uint32(77), got.CreatedLedger)
		assert.Equal(t, "GCREATOR", got.VerifiedBy)
		assert.Equal(t, "hash1", got.VerifiedWasmHash)
		require.NotNil(t, got.VerifiedAt)
	})

	t.Run("state is stored and replaced", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		// State is not part of the per-test reset, so the key is unique to
		// this run rather than assumed to be absent.
		k := fmt.Sprintf("test.%d", time.Now().UnixNano())

		_, err := s.GetState(ctx, k)
		assert.ErrorIs(t, err, store.ErrNotFound)

		require.NoError(t, s.SetState(ctx, k, "one"))
		require.NoError(t, s.SetState(ctx, k, "two"))
		got, err := s.GetState(ctx, k)
		require.NoError(t, err)
		assert.Equal(t, "two", got)
	})

	t.Run("saving a nil interface is rejected", func(t *testing.T) {
		s := newStore(t)

		err := s.Save(context.Background(), contract(idA, "testnet", "h1"), nil)
		require.Error(t, err)
	})

	t.Run("stored specs are independent of the caller's value", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		original := iface("original")
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), original))

		// Mutating what was passed in, or what was handed back, must not
		// disturb what the store holds.
		original.Functions[0].Name = "mutated"

		got, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		require.Equal(t, "original", got.Interface.Functions[0].Name)

		got.Interface.Functions[0].Name = "mutated-again"

		again, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "original", again.Interface.Functions[0].Name)
	})

	t.Run("round trips a rich interface", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		elem := model.Scalar(model.KindI128)
		rich := iface("complex")
		rich.Meta = map[string]string{"rsver": "1.93.0"}
		rich.Functions[0].Outputs = []model.Type{{
			Kind: model.KindVec, Element: &elem, Display: "Vec<i128>",
		}}
		rich.Types.Structs = []model.Struct{{
			Name:    "Pair",
			IsTuple: true,
			Fields: []model.StructField{
				{Name: "0", Type: model.Scalar(model.KindU32)},
				{Name: "1", Type: model.Scalar(model.KindU32)},
			},
		}}
		rich.Events = []model.Event{{
			Name:       "Transfer",
			DataFormat: "map",
			Params:     []model.EventParam{{Name: "from", Type: model.Scalar(model.KindAddress), Location: "topic-list"}},
		}}

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), rich))

		got, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, rich, got.Interface, "the stored ABI must survive a round trip unchanged")
	})

	t.Run("contracts are listed by the wasm they run", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("a")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h1"), iface("b")))

		page, err := s.ListContractsByWasm(ctx, store.WasmFilter{Network: "testnet", WasmHash: "h1"})
		require.NoError(t, err)
		assert.Equal(t, 2, page.Total)
		require.Len(t, page.Contracts, 2)
		for _, c := range page.Contracts {
			assert.True(t, c.Current, "both contracts run this hash")
		}

		page, err = s.ListContractsByWasm(ctx, store.WasmFilter{Network: "testnet", WasmHash: "h2"})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "no contract runs this hash")
		assert.Empty(t, page.Contracts, "an empty result must be [] rather than nil")
	})

	t.Run("the ever filter reaches contracts that ran the wasm in the past", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("a")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h2"), iface("a2")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h1"), iface("b")))

		page, err := s.ListContractsByWasm(ctx, store.WasmFilter{Network: "testnet", WasmHash: "h1"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total, "only the contract that still runs the hash is listed")
		assert.Equal(t, idB, page.Contracts[0].ContractID)

		page, err = s.ListContractsByWasm(ctx, store.WasmFilter{Network: "testnet", WasmHash: "h1", Ever: true})
		require.NoError(t, err)
		require.Equal(t, 2, page.Total, "the upgraded contract ran the hash too")
		for _, c := range page.Contracts {
			if c.ContractID == idA {
				assert.False(t, c.Current, "the upgraded contract no longer runs the hash")
			} else {
				assert.True(t, c.Current)
			}
		}
	})

	t.Run("the wasm listing is paged", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("a")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h1"), iface("b")))

		firstPage, err := s.ListContractsByWasm(ctx, store.WasmFilter{Network: "testnet", WasmHash: "h1", Limit: 1})
		require.NoError(t, err)
		require.Len(t, firstPage.Contracts, 1)
		assert.Equal(t, 2, firstPage.Total, "the total counts matches, not the page")

		secondPage, err := s.ListContractsByWasm(ctx, store.WasmFilter{Network: "testnet", WasmHash: "h1", Limit: 1, Offset: 1})
		require.NoError(t, err)
		require.Len(t, secondPage.Contracts, 1)
		assert.NotEqual(t, firstPage.Contracts[0].ContractID, secondPage.Contracts[0].ContractID,
			"the second page must hold the other contract")

		page, err := s.ListContractsByWasm(ctx, store.WasmFilter{Network: "testnet", WasmHash: "h1", Offset: 99})
		require.NoError(t, err)
		assert.Empty(t, page.Contracts)
		assert.Equal(t, 2, page.Total, "paging past the end still reports the true total")
	})

	t.Run("the wasm listing is scoped to one network", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("a")))
		require.NoError(t, s.Save(ctx, contract(idA, "public", "h1"), iface("b")))

		page, err := s.ListContractsByWasm(ctx, store.WasmFilter{Network: "public", WasmHash: "h1"})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total)

		page, err = s.ListContractsByWasm(ctx, store.WasmFilter{WasmHash: "h1"})
		require.NoError(t, err)
		assert.Equal(t, 2, page.Total, "an empty network means every network")
	})

	t.Run("deleting a contract removes every version", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("old")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h2"), iface("new")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h1"), iface("keep")))

		require.NoError(t, s.Delete(ctx, "testnet", idA))

		_, err := s.GetContract(ctx, "testnet", idA)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetSpec(ctx, "testnet", idA, "")
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.ListVersions(ctx, "testnet", idA)
		assert.ErrorIs(t, err, store.ErrNotFound, "the version history must go with the contract")

		_, err = s.GetContract(ctx, "testnet", idB)
		require.NoError(t, err, "deleting one contract must not touch another")

		assert.ErrorIs(t, s.Delete(ctx, "testnet", idA), store.ErrNotFound,
			"deleting an unknown contract reports not found")
	})

	t.Run("export and import round trip losslessly", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("old")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h2"), iface("new")))
		require.NoError(t, s.Save(ctx, contract(idB, "public", "h3"), iface("other")))
		require.NoError(t, s.MarkMissing(ctx, "testnet", idA, time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)))

		before, err := s.Export(ctx)
		require.NoError(t, err)
		assert.Equal(t, store.SnapshotVersion, before.Version)
		require.Len(t, before.Contracts, 2)

		for _, cs := range before.Contracts {
			require.NoError(t, s.Delete(ctx, cs.Contract.Network, cs.Contract.ContractID))
		}
		empty, err := s.ListContracts(ctx, store.ListFilter{})
		require.NoError(t, err)
		require.Zero(t, empty.Total)

		require.NoError(t, s.Import(ctx, before, false))

		after, err := s.Export(ctx)
		require.NoError(t, err)
		assert.Equal(t, before, after, "every contract, version and timestamp must survive the round trip")

		spec, err := s.GetSpec(ctx, "testnet", idA, "h1")
		require.NoError(t, err)
		assert.Equal(t, "old", spec.Interface.Functions[0].Name)
		versions, err := s.ListVersions(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Len(t, versions, 2, "the whole version history must be restored")
	})

	t.Run("import requires an empty registry unless merging", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("source")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h2"), iface("source-v2")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h1"), iface("source-b")))
		snapshot, err := s.Export(ctx)
		require.NoError(t, err)

		for _, cs := range snapshot.Contracts {
			require.NoError(t, s.Delete(ctx, cs.Contract.Network, cs.Contract.ContractID))
		}
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("existing")))

		assert.ErrorIs(t, s.Import(ctx, snapshot, false), store.ErrRegistryNotEmpty)

		require.NoError(t, s.Import(ctx, snapshot, true))
		got, err := s.GetSpec(ctx, "testnet", idA, "h1")
		require.NoError(t, err)
		assert.Equal(t, "existing", got.Interface.Functions[0].Name,
			"merge must not overwrite a contract that is already present")

		versions, err := s.ListVersions(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Len(t, versions, 2, "merge must add the versions that are missing")
	})

	t.Run("import rejects an unsupported snapshot version", func(t *testing.T) {
		s := newStore(t)
		err := s.Import(context.Background(), &store.Snapshot{Version: store.SnapshotVersion + 1}, false)
		assert.ErrorContains(t, err, "unsupported snapshot version")
	})

	t.Run("concurrent saves are safe", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		const workers = 8
		errs := make(chan error, workers)
		for i := range workers {
			go func() {
				id := fmt.Sprintf("%s%d", idA[:len(idA)-1], i)
				errs <- s.Save(ctx, contract(id, "testnet", "h1"), iface("f"))
			}()
		}
		for range workers {
			require.NoError(t, <-errs)
		}

		page, err := s.ListContracts(ctx, store.ListFilter{})
		require.NoError(t, err)
		assert.Equal(t, workers, page.Total)
	})

	t.Run("stats count contracts and versions per network", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		empty, err := s.Stats(ctx)
		require.NoError(t, err)
		assert.Empty(t, empty.Networks)
		assert.Zero(t, empty.Contracts)
		assert.Zero(t, empty.Versions)
		assert.Nil(t, empty.Latest)

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h2"), iface("g")))

		// Sleep so first_seen orders the two registrations unambiguously;
		// both stores timestamp with the database or wall clock.
		time.Sleep(5 * time.Millisecond)
		require.NoError(t, s.Save(ctx, contract(idB, "public", "h3"), iface("f")))

		stats, err := s.Stats(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.Contracts)
		assert.Equal(t, 3, stats.Versions, "a superseded interface still counts as a stored version")

		require.Len(t, stats.Networks, 2)
		assert.Equal(t, store.NetworkStats{Network: "public", Contracts: 1, Versions: 1}, stats.Networks[0])
		assert.Equal(t, store.NetworkStats{Network: "testnet", Contracts: 1, Versions: 2}, stats.Networks[1],
			"networks are ordered by name")

		require.NotNil(t, stats.Latest)
		assert.Equal(t, idB, stats.Latest.ContractID,
			"the most recently registered contract, not the most recently refreshed")
	})

	t.Run("upgrade feed excludes registrations and pages newest first", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "a1"), iface("f")))
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "a2"), iface("f")))
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "b1"), iface("f")))
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "b2"), iface("f")))
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "b3"), iface("f")))

		page, err := s.ListUpgrades(ctx, store.UpgradeFilter{})
		require.NoError(t, err)
		require.Equal(t, 3, page.Total, "each contract's first version is a registration, not an upgrade")
		require.Len(t, page.Upgrades, 3)

		assert.Equal(t, store.Upgrade{
			ContractID:  idB,
			Network:     "testnet",
			OldWasmHash: "b2",
			NewWasmHash: "b3",
			ObservedAt:  page.Upgrades[0].ObservedAt,
		}, page.Upgrades[0])
		assert.Equal(t, idB, page.Upgrades[1].ContractID)
		assert.Equal(t, idA, page.Upgrades[2].ContractID, "a2 is the oldest upgrade")

		first, err := s.ListUpgrades(ctx, store.UpgradeFilter{Limit: 1})
		require.NoError(t, err)
		require.Len(t, first.Upgrades, 1)
		assert.Equal(t, 3, first.Total)
		assert.Equal(t, "b3", first.Upgrades[0].NewWasmHash)

		second, err := s.ListUpgrades(ctx, store.UpgradeFilter{Limit: 1, Offset: 1})
		require.NoError(t, err)
		require.Len(t, second.Upgrades, 1)
		assert.Equal(t, "b2", second.Upgrades[0].NewWasmHash)

		past, err := s.ListUpgrades(ctx, store.UpgradeFilter{Offset: 99})
		require.NoError(t, err)
		assert.Empty(t, past.Upgrades)
		assert.Equal(t, 3, past.Total, "paging past the end still reports the true total")

		public, err := s.ListUpgrades(ctx, store.UpgradeFilter{Network: "public"})
		require.NoError(t, err)
		assert.Zero(t, public.Total)
		assert.Empty(t, public.Upgrades)

		require.NoError(t, s.Save(ctx, contract(idA, "public", "p1"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idA, "public", "p2"), iface("f")))

		public, err = s.ListUpgrades(ctx, store.UpgradeFilter{Network: "public"})
		require.NoError(t, err)
		require.Equal(t, 1, public.Total)
		assert.Equal(t, "p1", public.Upgrades[0].OldWasmHash)
		assert.Equal(t, "p2", public.Upgrades[0].NewWasmHash)
	})
}

func TestListFilterNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		in                    store.ListFilter
		wantLimit, wantOffset int
		wantQuery             string
	}{
		{"zero applies defaults", store.ListFilter{}, store.DefaultLimit, 0, ""},
		{"negative limit", store.ListFilter{Limit: -5}, store.DefaultLimit, 0, ""},
		{"oversized limit is clamped", store.ListFilter{Limit: 5000}, store.MaxLimit, 0, ""},
		{"negative offset", store.ListFilter{Offset: -3}, store.DefaultLimit, 0, ""},
		{"query is trimmed", store.ListFilter{Query: "  abc \n"}, store.DefaultLimit, 0, "abc"},
		{"valid values pass through", store.ListFilter{Limit: 10, Offset: 20}, 10, 20, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := tt.in
			f.Normalize()
			assert.Equal(t, tt.wantLimit, f.Limit)
			assert.Equal(t, tt.wantOffset, f.Offset)
			assert.Equal(t, tt.wantQuery, f.Query)
		})
	}
}

func mustContract(t *testing.T, s store.Store, id string) *store.Contract {
	t.Helper()
	got, err := s.GetContract(context.Background(), "testnet", id)
	require.NoError(t, err)
	return got
}
