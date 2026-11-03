package refresher

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/soroworks/sorovault/internal/store"
)

const (
	// DefaultBrokerBuffer is the per-subscriber queue depth when none is
	// specified. A subscriber that falls further behind than this loses its
	// oldest pending events rather than stalling the publisher.
	DefaultBrokerBuffer = 64

	// DefaultMaxSubscribers is the maximum number of concurrent SSE
	// subscribers a Broker accepts when no explicit limit is given.
	DefaultMaxSubscribers = 256
)

// ErrTooManySubscribers reports that the broker is already serving its
// maximum number of concurrent subscribers.
var ErrTooManySubscribers = errors.New("refresher: too many stream subscribers")

// StreamFilter selects which upgrade events a subscriber receives. Empty
// fields match every upgrade.
type StreamFilter struct {
	Network    string
	ContractID string
}

// Matches reports whether u satisfies the filter.
func (f StreamFilter) Matches(u store.Upgrade) bool {
	if f.Network != "" && u.Network != f.Network {
		return false
	}
	if f.ContractID != "" && u.ContractID != f.ContractID {
		return false
	}
	return true
}

// Broker fans recorded contract upgrades out to in-process SSE subscribers.
// All methods are safe for concurrent use.
type Broker struct {
	mu             sync.Mutex
	subs           map[*Subscriber]struct{}
	maxSubscribers int
	buffer         int
	dropped        atomic.Uint64
}

// NewBroker returns a Broker whose subscribers each buffer up to buffer
// upgrade events, capped at maxSubscribers concurrent clients. Non-positive
// values fall back to DefaultMaxSubscribers and DefaultBrokerBuffer.
func NewBroker(maxSubscribers, buffer int) *Broker {
	if maxSubscribers < 1 {
		maxSubscribers = DefaultMaxSubscribers
	}
	if buffer < 1 {
		buffer = DefaultBrokerBuffer
	}
	return &Broker{
		subs:           make(map[*Subscriber]struct{}),
		maxSubscribers: maxSubscribers,
		buffer:         buffer,
	}
}

// SetMaxSubscribers updates the concurrent subscriber limit. Values below one
// fall back to DefaultMaxSubscribers.
func (b *Broker) SetMaxSubscribers(maxSubscribers int) {
	if maxSubscribers < 1 {
		maxSubscribers = DefaultMaxSubscribers
	}
	b.mu.Lock()
	b.maxSubscribers = maxSubscribers
	b.mu.Unlock()
}

// SetBuffer updates the per-subscriber channel capacity for future
// subscribers. Values below one fall back to DefaultBrokerBuffer.
func (b *Broker) SetBuffer(buffer int) {
	if buffer < 1 {
		buffer = DefaultBrokerBuffer
	}
	b.mu.Lock()
	b.buffer = buffer
	b.mu.Unlock()
}

// Subscriber is one connected SSE client. Read upgrades from C until the
// client disconnects, then Close it.
type Subscriber struct {
	// C delivers upgrades matching the subscriber's filter.
	C <-chan store.Upgrade

	b         *Broker
	ch        chan store.Upgrade
	filter    StreamFilter
	closeOnce sync.Once
}

// Subscribe registers a new subscriber matching filter. It returns
// ErrTooManySubscribers when the broker is at its connection limit.
func (b *Broker) Subscribe(filter StreamFilter) (*Subscriber, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxSubscribers > 0 && len(b.subs) >= b.maxSubscribers {
		return nil, ErrTooManySubscribers
	}

	ch := make(chan store.Upgrade, b.buffer)
	sub := &Subscriber{
		C:      ch,
		b:      b,
		ch:     ch,
		filter: filter,
	}
	b.subs[sub] = struct{}{}
	return sub, nil
}

// Close unregisters the subscriber. It is idempotent.
func (s *Subscriber) Close() {
	s.closeOnce.Do(func() {
		s.b.mu.Lock()
		delete(s.b.subs, s)
		s.b.mu.Unlock()
	})
}

// Publish sends u to every matching subscriber without blocking. When a
// subscriber's buffer is full, its oldest pending upgrade is discarded to
// make room for the newest and Dropped is incremented.
func (b *Broker) Publish(u store.Upgrade) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for sub := range b.subs {
		if !sub.filter.Matches(u) {
			continue
		}
		b.deliver(sub, u)
	}
}

// deliver enqueues u for one subscriber under b.mu, dropping the oldest
// queued event if the subscriber's buffer is full.
func (b *Broker) deliver(sub *Subscriber, u store.Upgrade) {
	select {
	case sub.ch <- u:
		return
	default:
	}

	select {
	case <-sub.ch:
		b.dropped.Add(1)
	default:
	}

	select {
	case sub.ch <- u:
	default:
		b.dropped.Add(1)
	}
}

// SubscriberCount returns the number of currently registered subscribers.
func (b *Broker) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Dropped returns the total number of events discarded because a subscriber
// could not keep up.
func (b *Broker) Dropped() uint64 {
	return b.dropped.Load()
}
