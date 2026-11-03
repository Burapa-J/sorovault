package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/api"
	"github.com/soroworks/sorovault/internal/refresher"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
)

// upgradeFixture registers one contract and upgrades it twice, with a clock
// that advances on every store write so the events order deterministically.
func upgradeFixture(t *testing.T) (*harness, []string) {
	t.Helper()

	memory := store.NewMemory()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	memory.SetClock(func() time.Time {
		now = now.Add(time.Minute)
		return now
	})
	h := newHarnessWithStore(t, stellartest.New(), memory)

	v1 := moduleWith(t, "v1")
	h.fake.Deploy(idA, v1)
	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	v2 := moduleWith(t, "v2")
	h.fake.Upgrade(idA, v2)
	resp, raw = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	v3 := moduleWith(t, "v3")
	h.fake.Upgrade(idA, v3)
	resp, raw = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	return h, []string{stellartest.HashOf(v1), stellartest.HashOf(v2), stellartest.HashOf(v3)}
}

func TestUpgradesFeed(t *testing.T) {
	t.Parallel()

	h, hashes := upgradeFixture(t)

	t.Run("first registration is not an upgrade", func(t *testing.T) {
		// The fixture has two upgrades, not three: the initial version is
		// how the contract entered the registry.
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, float64(2), got["total"])
		assert.Len(t, got["upgrades"].([]any), 2)
	})

	t.Run("newest first with both hashes", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		upgrades := h.decode(t, raw)["upgrades"].([]any)
		require.Len(t, upgrades, 2)

		newest := upgrades[0].(map[string]any)
		assert.Equal(t, idA, newest["contract_id"])
		assert.Equal(t, "testnet", newest["network"])
		assert.Equal(t, hashes[1], newest["old_wasm_hash"])
		assert.Equal(t, hashes[2], newest["new_wasm_hash"])
		assert.NotEmpty(t, newest["observed_at"])

		older := upgrades[1].(map[string]any)
		assert.Equal(t, hashes[0], older["old_wasm_hash"])
		assert.Equal(t, hashes[1], older["new_wasm_hash"])
	})

	t.Run("paging", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades?limit=1&offset=1", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, float64(2), got["total"], "total ignores paging")
		assert.Equal(t, float64(1), got["limit"])
		assert.Equal(t, float64(1), got["offset"])

		upgrades := got["upgrades"].([]any)
		require.Len(t, upgrades, 1)
		assert.Equal(t, hashes[1], upgrades[0].(map[string]any)["new_wasm_hash"],
			"the second page holds the older upgrade")
	})

	t.Run("past the end still reports the total", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades?offset=99", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, []any{}, got["upgrades"])
		assert.Equal(t, float64(2), got["total"])
	})

	t.Run("network filter", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades?network=public", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, float64(0), got["total"])
		assert.Equal(t, []any{}, got["upgrades"])
	})
}

// TestUpgradesAtomFeed proves the Atom form carries the same events with the
// fields a feed reader needs, and that paging is linked.
func TestUpgradesAtomFeed(t *testing.T) {
	t.Parallel()

	h, hashes := upgradeFixture(t)

	type atomDocument struct {
		ID      string `xml:"id"`
		Title   string `xml:"title"`
		Updated string `xml:"updated"`
		Links   []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
		Entries []struct {
			ID      string `xml:"id"`
			Title   string `xml:"title"`
			Updated string `xml:"updated"`
			Summary string `xml:"summary"`
			Link    struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}

	resp, raw := h.do(t, http.MethodGet, "/api/upgrades.atom", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/atom+xml")

	var feed atomDocument
	require.NoError(t, xml.Unmarshal(raw, &feed), "the feed must be well-formed Atom XML")

	assert.Equal(t, "urn:sorovault:upgrades", feed.ID)
	assert.NotEmpty(t, feed.Updated)
	require.Len(t, feed.Entries, 2)

	newest := feed.Entries[0]
	assert.Contains(t, newest.Title, idA)
	assert.Contains(t, newest.Summary, hashes[1])
	assert.Contains(t, newest.Summary, hashes[2])
	assert.NotEmpty(t, newest.Updated)
	_, err := time.Parse(time.RFC3339, newest.Updated)
	assert.NoError(t, err, "Atom requires an RFC 3339 timestamp")
	assert.Contains(t, newest.Link.Href, "/api/contracts/"+idA)

	assert.Contains(t, feed.Entries[1].Summary, hashes[0],
		"entries are newest first, like the JSON feed")

	resp, raw = h.do(t, http.MethodGet, "/api/upgrades.atom?limit=1", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	feed = atomDocument{}
	require.NoError(t, xml.Unmarshal(raw, &feed))
	require.Len(t, feed.Entries, 1)

	var next string
	for _, link := range feed.Links {
		if link.Rel == "next" {
			next = link.Href
		}
	}
	assert.Contains(t, next, "offset=1", "a page with more events links to the next one")
}

type sseUpgradeEvent struct {
	ID    string
	Event string
	Data  store.Upgrade
}

func openUpgradeStream(t *testing.T, h *harness, path, lastEventID string) (*http.Response, <-chan sseUpgradeEvent, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := h.server.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	events := make(chan sseUpgradeEvent, 16)
	go func() {
		defer close(events)
		defer func() { _ = resp.Body.Close() }()

		scanner := bufio.NewScanner(resp.Body)
		var cur sseUpgradeEvent
		var rawData string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, ":") {
				continue
			}
			if line == "" {
				if rawData != "" {
					var u store.Upgrade
					if json.Unmarshal([]byte(rawData), &u) == nil {
						cur.Data = u
						select {
						case events <- cur:
						case <-ctx.Done():
							return
						}
					}
				}
				cur = sseUpgradeEvent{}
				rawData = ""
				continue
			}
			if v, ok := strings.CutPrefix(line, "id: "); ok {
				cur.ID = v
			} else if v, ok := strings.CutPrefix(line, "event: "); ok {
				cur.Event = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				rawData = v
			}
		}
	}()

	t.Cleanup(cancel)
	return resp, events, cancel
}

func readSSEUpgrade(t *testing.T, ch <-chan sseUpgradeEvent) sseUpgradeEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		require.True(t, ok, "SSE stream closed before delivering upgrade")
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE upgrade event")
		return sseUpgradeEvent{}
	}
}

func TestUpgradesStreamLiveDelivery(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	memory.SetClock(func() time.Time {
		now = now.Add(time.Minute)
		return now
	})
	fake := stellartest.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := registry.New(fake, spec.XDRDecoder{}, memory, registry.Options{Logger: log})

	ref, err := refresher.New(reg, refresher.Options{
		Interval: time.Hour,
		Logger:   log,
		Now:      func() time.Time { return now },
	})
	require.NoError(t, err)

	h := newHarnessWithConfigure(t, func(s *api.Server) {
		s.SetBroker(ref.Broker())
	})
	// Share the same fake and registry with the refresher by registering via h.
	v1 := moduleWith(t, "v1")
	h.fake.Deploy(idA, v1)
	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	streamResp, events, cancel := openUpgradeStream(t, h, "/api/upgrades/stream", "")
	defer cancel()

	assert.Equal(t, "text/event-stream", streamResp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", streamResp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", streamResp.Header.Get("X-Accel-Buffering"))
	assert.Empty(t, streamResp.Header.Get("Content-Encoding"), "SSE stream must not be gzipped")

	v2 := moduleWith(t, "v2")
	h.fake.Upgrade(idA, v2)
	resp, raw = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	ev := readSSEUpgrade(t, events)
	assert.Equal(t, "upgrade", ev.Event)
	assert.Equal(t, idA, ev.Data.ContractID)
	assert.Equal(t, "testnet", ev.Data.Network)
	assert.Equal(t, stellartest.HashOf(v1), ev.Data.OldWasmHash)
	assert.Equal(t, stellartest.HashOf(v2), ev.Data.NewWasmHash)
	assert.Equal(t, api.UpgradeEventID(ev.Data), ev.ID)

	// Also verify refresher.Sweep publishes directly through its shared Broker.
	fake.Deploy(idB, v1)
	_, err = reg.Register(context.Background(), idB)
	require.NoError(t, err)
	now = now.Add(2 * time.Hour)
	v3 := moduleWith(t, "v3")
	fake.Upgrade(idB, v3)
	sum, err := ref.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Changed)

	ev2 := readSSEUpgrade(t, events)
	assert.Equal(t, idB, ev2.Data.ContractID)
	assert.Equal(t, stellartest.HashOf(v1), ev2.Data.OldWasmHash)
	assert.Equal(t, stellartest.HashOf(v3), ev2.Data.NewWasmHash)
}

func TestUpgradesStreamLastEventIDReplay(t *testing.T) {
	t.Parallel()

	h, hashes := upgradeFixture(t)

	v4 := moduleWith(t, "v4")
	h.fake.Upgrade(idA, v4)
	resp, raw := h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	hashV4 := stellartest.HashOf(v4)

	// Replay from the first upgrade (v1 -> v2); the stream should replay
	// v2 -> v3 and v3 -> v4 in chronological order, then deliver live v4 -> v5.
	firstUpgradeID := api.UpgradeEventID(store.Upgrade{
		Network:     "testnet",
		ContractID:  idA,
		OldWasmHash: hashes[0],
		NewWasmHash: hashes[1],
	})

	_, events, cancel := openUpgradeStream(t, h, "/api/upgrades/stream", firstUpgradeID)
	defer cancel()

	replayed1 := readSSEUpgrade(t, events)
	assert.Equal(t, hashes[1], replayed1.Data.OldWasmHash)
	assert.Equal(t, hashes[2], replayed1.Data.NewWasmHash)

	replayed2 := readSSEUpgrade(t, events)
	assert.Equal(t, hashes[2], replayed2.Data.OldWasmHash)
	assert.Equal(t, hashV4, replayed2.Data.NewWasmHash)

	v5 := moduleWith(t, "v5")
	h.fake.Upgrade(idA, v5)
	resp, raw = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	live := readSSEUpgrade(t, events)
	assert.Equal(t, hashV4, live.Data.OldWasmHash)
	assert.Equal(t, stellartest.HashOf(v5), live.Data.NewWasmHash)
}

func TestUpgradesStreamFilters(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	resp, raw := h.do(t, http.MethodGet, "/api/upgrades/stream?contract_id=invalid", "")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))

	v1 := moduleWith(t, "v1")
	v2 := moduleWith(t, "v2")
	h.fake.Deploy(idA, v1)
	h.fake.Deploy(idB, v1)
	_, _ = h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	_, _ = h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idB+`"}`)

	_, eventsB, cancelB := openUpgradeStream(t, h, "/api/upgrades/stream?contract_id="+idB, "")
	defer cancelB()

	_, eventsPub, cancelPub := openUpgradeStream(t, h, "/api/upgrades/stream?network=public", "")
	defer cancelPub()

	// Upgrade idA first; the idB stream must ignore it, then receive idB's upgrade.
	h.fake.Upgrade(idA, v2)
	_, _ = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")

	h.fake.Upgrade(idB, v2)
	_, _ = h.do(t, http.MethodPost, "/api/contracts/"+idB+"/refresh", "")

	ev := readSSEUpgrade(t, eventsB)
	assert.Equal(t, idB, ev.Data.ContractID)

	select {
	case unexpected := <-eventsPub:
		t.Fatalf("public network stream received testnet upgrade: %+v", unexpected)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestUpgradesStreamDisconnectCleansUpSubscriber(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	assert.Equal(t, 0, h.api.Broker().SubscriberCount())

	_, _, cancel1 := openUpgradeStream(t, h, "/api/upgrades/stream", "")
	_, _, cancel2 := openUpgradeStream(t, h, "/api/upgrades/stream", "")
	require.Eventually(t, func() bool {
		return h.api.Broker().SubscriberCount() == 2
	}, time.Second, 10*time.Millisecond)

	cancel1()
	require.Eventually(t, func() bool {
		return h.api.Broker().SubscriberCount() == 1
	}, time.Second, 10*time.Millisecond)

	cancel2()
	require.Eventually(t, func() bool {
		return h.api.Broker().SubscriberCount() == 0
	}, time.Second, 10*time.Millisecond)
}

func TestUpgradesStreamMaxSubscribersLimit(t *testing.T) {
	t.Parallel()

	h := newHarnessWithConfigure(t, func(s *api.Server) {
		s.SetStreamMaxSubscribers(2)
	})

	_, _, cancel1 := openUpgradeStream(t, h, "/api/upgrades/stream", "")
	defer cancel1()
	_, _, cancel2 := openUpgradeStream(t, h, "/api/upgrades/stream", "")
	defer cancel2()

	require.Eventually(t, func() bool {
		return h.api.Broker().SubscriberCount() == 2
	}, time.Second, 10*time.Millisecond)

	resp, raw := h.do(t, http.MethodGet, "/api/upgrades/stream", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))

	cancel1()
	require.Eventually(t, func() bool {
		return h.api.Broker().SubscriberCount() == 1
	}, time.Second, 10*time.Millisecond)

	_, _, cancel3 := openUpgradeStream(t, h, "/api/upgrades/stream", "")
	defer cancel3()
}

func TestUpgradesStreamSlowClientDropsOldest(t *testing.T) {
	t.Parallel()

	broker := refresher.NewBroker(10, 2)
	sub, err := broker.Subscribe(refresher.StreamFilter{})
	require.NoError(t, err)
	defer sub.Close()

	for _, hash := range []string{"h1", "h2", "h3", "h4", "h5"} {
		broker.Publish(store.Upgrade{
			ContractID:  idA,
			Network:     "testnet",
			OldWasmHash: "h0",
			NewWasmHash: hash,
		})
	}

	assert.Equal(t, uint64(3), broker.Dropped(), "three oldest events should be dropped when buffer of 2 overflows")
	first := <-sub.C
	second := <-sub.C
	assert.Equal(t, "h4", first.NewWasmHash)
	assert.Equal(t, "h5", second.NewWasmHash)
}

func TestUpgradesStreamHeartbeat(t *testing.T) {
	t.Parallel()

	h := newHarnessWithConfigure(t, func(s *api.Server) {
		s.SetStreamHeartbeat(20 * time.Millisecond)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+"/api/upgrades/stream", nil)
	require.NoError(t, err)
	resp, err := h.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	require.True(t, scanner.Scan(), "expected a heartbeat line from the stream")
	assert.Equal(t, ": heartbeat", scanner.Text())
}
