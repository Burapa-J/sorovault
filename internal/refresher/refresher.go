// Package refresher re-checks registered contracts on a schedule, so the
// registry notices an upgraded contract without anyone asking it to.
//
// A sweep lists every contract on the registry's network whose
// last_refreshed is older than the interval and calls Registry.Refresh on
// each. Refresh settles an unchanged contract with a single RPC round trip,
// so a sweep over a mostly-static registry is cheap.
package refresher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
)

const (
	initialRateLimitBackoff = time.Second
	maxRateLimitBackoff     = 16 * time.Second
	maxRateLimitRetries     = 5
)

// Registry is the slice of *registry.Registry a sweep needs. It is an
// interface so the scheduling can be tested without a network.
type Registry interface {
	Network(ctx context.Context) (string, error)
	Refresh(ctx context.Context, contractID string) (*registry.Result, error)
	Store() store.Store
}

// Options configures a Refresher.
type Options struct {
	// Interval is both how often a sweep runs and how stale a contract must
	// be before a sweep re-checks it. Must be positive.
	Interval time.Duration
	// Timeout bounds the refresh of a single contract, so one slow RPC
	// response cannot stall the rest of the sweep. Zero means no bound
	// beyond the sweep's own context.
	Timeout time.Duration
	// Notifier, when set, is told about every contract whose interface
	// changed during a sweep. Delivery is best effort.
	Notifier Notifier
	// Broker, when set, fans upgrade events out to in-process SSE
	// subscribers. When nil, New creates one with default limits.
	Broker *Broker
	// Logger receives one line per sweep and one per failure. Defaults to
	// slog.Default.
	Logger *slog.Logger
	// Now is the clock; tests replace it. Defaults to time.Now.
	Now func() time.Time
	// Events, when set, makes the refresher follow the network's
	// contract-upgrade events and refresh a contract as soon as it is
	// upgraded. Polling continues alongside as a slow safety net.
	Events stellar.UpdateFollower
	// EventsInterval is how often new events are read. Defaults to ten
	// seconds.
	EventsInterval time.Duration
	// Wait pauses between scheduled refreshes. Tests replace it to avoid
	// sleeping; the default respects context cancellation.
	Wait func(context.Context, time.Duration) error
}

// Refresher runs periodic sweeps over the registry.
type Refresher struct {
	reg      Registry
	interval time.Duration
	timeout  time.Duration
	notifier Notifier
	broker   *Broker
	log      *slog.Logger
	now      func() time.Time
	wait     func(context.Context, time.Duration) error

	events         stellar.UpdateFollower
	eventsInterval time.Duration

	mu        sync.Mutex
	lastSweep *SweepRecord
}

// New builds a Refresher. It returns an error for a non-positive interval
// rather than spinning.
func New(reg Registry, opts Options) (*Refresher, error) {
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("refresher: interval must be positive, got %s", opts.Interval)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	wait := opts.Wait
	if wait == nil {
		wait = waitContext
	}
	eventsInterval := opts.EventsInterval
	if eventsInterval <= 0 {
		eventsInterval = defaultEventsInterval
	}
	broker := opts.Broker
	if broker == nil {
		broker = NewBroker(0, 0)
	}
	return &Refresher{
		events:         opts.Events,
		eventsInterval: eventsInterval,
		reg:            reg,
		interval:       opts.Interval,
		timeout:        opts.Timeout,
		notifier:       opts.Notifier,
		broker:         broker,
		log:            log,
		now:            now,
		wait:           wait,
	}, nil
}

// Broker returns the in-process upgrade fan-out broker.
func (r *Refresher) Broker() *Broker {
	return r.broker
}

// SweepRecord is what the most recent completed sweep did and when it
// finished. Serve reports it at GET /api/stats; it is deliberately in
// memory, since a restart simply starts a new sweep.
type SweepRecord struct {
	Summary
	CompletedAt time.Time `json:"completed_at"`
}

// LastSweep reports the most recent completed sweep. The second result is
// false until a sweep has finished.
func (r *Refresher) LastSweep() (SweepRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastSweep == nil {
		return SweepRecord{}, false
	}
	return *r.lastSweep, true
}

// Summary reports what one sweep did.
type Summary struct {
	// Checked is how many stale contracts the sweep attempted.
	Checked int `json:"checked"`
	// Changed is how many of them had been upgraded since the last check.
	Changed int `json:"changed"`
	// Failed is how many could not be refreshed. Their errors are logged.
	Failed int `json:"failed"`
	// Busy is how many were not attempted because the registry was at its
	// concurrency limit. They stay stale, so the next sweep picks them up.
	Busy int `json:"busy"`
	// Missing is how many are no longer on the network. They are not
	// failures: the registry keeps what it knows about them, and says when
	// they went away.
	Missing int `json:"missing"`
}

// Run sweeps once immediately and then every interval until ctx is
// cancelled. It returns nil on cancellation; a failed sweep is logged and
// retried on the next tick rather than ending the loop, because a refresher
// that dies on the first RPC outage is worse than none.
func (r *Refresher) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	if r.events != nil {
		followCtx, stopFollowing := context.WithCancel(ctx)
		followed := make(chan struct{})
		go func() {
			defer close(followed)
			r.FollowEvents(followCtx)
		}()
		defer func() {
			stopFollowing()
			<-followed
		}()
	}

	for {
		r.sweepAndLog(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Refresher) sweepAndLog(ctx context.Context) {
	start := r.now()
	sum, err := r.Sweep(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.WarnContext(ctx, "refresh sweep failed")
		}
		return
	}

	record := SweepRecord{Summary: sum, CompletedAt: r.now().UTC()}
	r.mu.Lock()
	r.lastSweep = &record
	r.mu.Unlock()

	r.log.InfoContext(ctx, "refresh sweep complete",
		"checked", sum.Checked,
		"changed", sum.Changed,
		"missing", sum.Missing,
		"failed", sum.Failed,
		"took", r.now().Sub(start).Round(time.Millisecond),
	)
}

// Sweep refreshes every contract on the registry's network that has not
// been refreshed within the interval.
//
// The stale set is collected in full before any refresh runs. Listings are
// ordered by last_refreshed, and refreshing a contract moves it to the front,
// so paging while refreshing would skip some contracts and revisit others.
//
// A failure on one contract is counted and logged, and the sweep moves on.
// A contract that has gone from the network is marked missing rather than
// counted as a failure. Sweep only returns an error when it cannot work out
// what to refresh.
func (r *Refresher) Sweep(ctx context.Context) (Summary, error) {
	return r.sweep(ctx, false)
}

// SweepAll refreshes every contract on the registry's network exactly once,
// regardless of how recently it was refreshed. It is what the CLI's
// `refresh --all` runs: one manual sweep, not a scheduled one.
//
// Unlike Sweep it does not pace refreshes across the interval, because a
// command that waited an interval before finishing would be useless as a
// cron job or an after-outage recovery step.
func (r *Refresher) SweepAll(ctx context.Context) (Summary, error) {
	return r.sweep(ctx, true)
}

func (r *Refresher) sweep(ctx context.Context, all bool) (Summary, error) {
	var sum Summary

	network, err := r.reg.Network(ctx)
	if err != nil {
		return sum, err
	}

	stale, err := r.staleContracts(ctx, network, all)
	if err != nil {
		return sum, err
	}
	if len(stale) == 0 {
		return sum, nil
	}

	started := r.now()
	count := int64(len(stale))
	step := r.interval / time.Duration(count)
	remainder := int64(r.interval % time.Duration(count))
	var target time.Duration
	var remainderProgress int64
	rateLimitBackoff := initialRateLimitBackoff
	rateLimitLogged := false
	for i, contract := range stale {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if !all && i > 0 {
			target += step
			remainderProgress += remainder
			if remainderProgress >= count {
				target++
				remainderProgress -= count
			}
			if delay := target - r.now().Sub(started); delay > 0 {
				if err := r.wait(ctx, delay); err != nil {
					return sum, err
				}
			}
		}

		sum.Checked++
		var result *registry.Result
		var rateLimitRetries int
		for {
			result, err = r.refreshOne(ctx, contract.ContractID)
			if !errors.Is(err, stellar.ErrRateLimited) {
				break
			}

			if !rateLimitLogged {
				r.log.WarnContext(ctx, "RPC rate-limited refresh sweep; backing off",
					"network", stellar.NetworkLogLabel(network), "delay", rateLimitBackoff)
				rateLimitLogged = true
			} else {
				r.log.DebugContext(ctx, "RPC still rate-limiting refresh sweep",
					"network", stellar.NetworkLogLabel(network),
					"delay", rateLimitBackoff)
			}

			if rateLimitRetries == maxRateLimitRetries {
				sum.Failed++
				return sum, nil
			}
			if err := r.wait(ctx, rateLimitBackoff); err != nil {
				return sum, err
			}
			rateLimitRetries++
			if rateLimitBackoff < maxRateLimitBackoff {
				rateLimitBackoff *= 2
				if rateLimitBackoff > maxRateLimitBackoff {
					rateLimitBackoff = maxRateLimitBackoff
				}
			}
		}
		rateLimitBackoff = initialRateLimitBackoff
		r.recordRefresh(ctx, network, contract, result, err, &sum)
	}

	return sum, nil
}

// refreshContract refreshes one contract and records the outcome in sum:
// the counters and the log line a sweep and the event follower share.
func (r *Refresher) refreshContract(ctx context.Context, network string, contract store.Contract, sum *Summary) {
	sum.Checked++
	result, err := r.refreshOne(ctx, contract.ContractID)
	r.recordRefresh(ctx, network, contract, result, err, sum)
}

// recordRefresh records the outcome of refreshing one contract in sum: the
// counters and the log line a sweep and the event follower share. The caller
// has already counted the contract as checked.
func (r *Refresher) recordRefresh(ctx context.Context, network string, contract store.Contract, result *registry.Result, err error, sum *Summary) {
	if err != nil {
		// A busy registry is not a failure: the contract stays stale
		// and the next sweep tries again. Failing it would report a
		// problem where there is none.
		if errors.Is(err, registry.ErrBusy) {
			sum.Busy++
			r.log.DebugContext(ctx, "refresh deferred, registry busy")
			return
		}
		sum.Failed++
		r.log.WarnContext(ctx, "refresh failed")
		return
	}

	if result.Missing {
		// The contract is gone from the network. That is not a
		// failure — the registry still knows what it was — but it is
		// worth saying once, when it happens, rather than on every
		// sweep for as long as the contract stays away.
		sum.Missing++
		if contract.MissingSince == nil {
			r.log.WarnContext(ctx, "contract no longer on the network",
				"network", stellar.NetworkLogLabel(network), "missing_since", result.Contract.MissingSince)
		} else {
			r.log.DebugContext(ctx, "contract still missing",
				"network", stellar.NetworkLogLabel(network),
				"missing_since", contract.MissingSince)
		}
		return
	}

	if result.Changed {
		sum.Changed++
		r.notify(ctx, network, result)
	}
}

// notify tells the notifier that a contract's interface changed.
//
// A delivery that fails is logged and otherwise ignored: the sweep exists to
// keep the registry up to date, and how fast a receiver reads about it is
// the receiver's business.
func (r *Refresher) notify(ctx context.Context, network string, result *registry.Result) {
	observedAt := result.Contract.LastRefreshed.UTC()
	if observedAt.IsZero() {
		observedAt = r.now().UTC()
	}
	if r.broker != nil {
		r.broker.Publish(store.Upgrade{
			ContractID:  result.Contract.ContractID,
			Network:     network,
			OldWasmHash: result.PreviousWasmHash,
			NewWasmHash: result.Contract.CurrentWasmHash,
			ObservedAt:  observedAt,
		})
	}

	change := Change{
		ContractID:   result.Contract.ContractID,
		Network:      network,
		OldWasmHash:  result.PreviousWasmHash,
		NewWasmHash:  result.Contract.CurrentWasmHash,
		InterfaceURL: interfaceURL(result.Contract.ContractID, network, result.Contract.CurrentWasmHash),
		ChangedAt:    r.now().UTC(),
	}

	r.notifySubscribers(ctx, change)
	if r.notifier == nil {
		return
	}

	if err := r.notifier.Notify(ctx, change); err != nil {
		r.log.WarnContext(ctx, "refresh webhook failed")
	}
}

// notifySubscribers delivers a change to every active subscription on the
// change's network that covers the contract, signed with the subscription's
// secret. A failed delivery is logged like the global webhook's: the sweep
// does not wait on a receiver.
func (r *Refresher) notifySubscribers(ctx context.Context, change Change) {
	db := r.reg.Store()
	items, err := db.ListSubscriptions(ctx)
	if err != nil {
		r.log.WarnContext(ctx, "listing webhook subscriptions failed")
		return
	}
	for _, item := range items {
		if !item.Active || item.Network != change.Network {
			continue
		}
		sub, err := db.GetSubscription(ctx, item.ID)
		if err != nil {
			r.log.WarnContext(ctx, "reading webhook subscription failed")
			continue
		}
		if !slices.Contains(sub.ContractIDs, change.ContractID) {
			continue
		}
		if err := deliverWebhook(ctx, sub.URL, sub.ID, change, r.now, r.log, nil, 0); err != nil {
			r.log.WarnContext(ctx, "subscription webhook failed")
		}
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Refresher) refreshOne(ctx context.Context, id string) (*registry.Result, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	return r.reg.Refresh(ctx, id)
}

// staleContracts lists the contracts to refresh. Unless all is set, a
// contract must have been last refreshed before now minus the interval.
//
// The whole record is kept, not just the ID: the sweep needs to know whether
// a contract was already known to be missing before it refreshes it.
func (r *Refresher) staleContracts(ctx context.Context, network string, all bool) ([]store.Contract, error) {
	cutoff := r.now().Add(-r.interval)

	var contracts []store.Contract
	filter := store.ListFilter{Network: network, Limit: store.MaxLimit}
	for {
		page, err := r.reg.Store().ListContracts(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("refresher: listing contracts: %w", err)
		}
		for _, c := range page.Contracts {
			if all || c.LastRefreshed.Before(cutoff) {
				contracts = append(contracts, c)
			}
		}

		filter.Offset += len(page.Contracts)
		if len(page.Contracts) == 0 || filter.Offset >= page.Total {
			return contracts, nil
		}
	}
}
