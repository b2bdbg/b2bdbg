package capture

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

// lruEntryOf extracts the *lruEntry stored on an LRU list element. The
// invariant is that every value placed into Store.list is *lruEntry — this
// helper makes that invariant a single asserted-and-checked site (a
// comma-ok'd type assertion that panics with a clear diagnostic if it is
// ever violated) instead of a scattered set of unchecked `.(*lruEntry)`
// casts that errcheck rightly flags as panic risks.
func lruEntryOf(el *list.Element) *lruEntry {
	e, ok := el.Value.(*lruEntry)
	if !ok {
		panic(fmt.Sprintf("capture: lru element has unexpected type %T (want *lruEntry) — programmer error", el.Value))
	}
	return e
}

// -----------------------------------------------------------------------
// Store config
// -----------------------------------------------------------------------

// StoreConfig controls the bounded-memory and loop-detection behaviour of a
// [Store].
type StoreConfig struct {
	// MaxConversations is the maximum number of live conversations held in the
	// LRU cache. When the limit is exceeded, the least-recently-used entry is
	// evicted. Zero means use [DefaultMaxConversations].
	MaxConversations int

	// ConvTTL is how long a conversation is retained after its last event
	// before being eligible for TTL eviction. Zero means use [DefaultConvTTL].
	ConvTTL time.Duration

	// LoopWindowSize is the number of recent events examined per conversation
	// when looking for repeating cycles. Zero means use [DefaultLoopWindow].
	LoopWindowSize int

	// LoopEntryTTL is how long a loop-window entry is retained. Zero means use
	// [DefaultLoopTTL].
	LoopEntryTTL time.Duration
}

// DefaultMaxConversations is the default maximum number of concurrent
// conversations the store tracks.
const DefaultMaxConversations = 10_000

// DefaultConvTTL is the default time after the last event before a conversation
// is considered stale and evicted.
const DefaultConvTTL = 30 * time.Minute

// -----------------------------------------------------------------------
// Listener
// -----------------------------------------------------------------------

// Listener is called synchronously inside [Store.Record] after a [ParsedMessage]
// has been correlated into a [Conversation].
//
// OnEvent is invoked AFTER the Store lock has been released (not while it is
// held), so a listener must not call back into the Store, must be fast and
// non-blocking, and may be called from any goroutine.
//
// Because the lock is not held during the callback, conv must be treated as
// read-only and must NOT be retained beyond the call: the Store may append a
// subsequent event to the same *Conversation concurrently. Copy any field you
// need (conv.ID and the passed-by-value ev are stable) rather than holding the
// pointer.
type Listener interface {
	// OnEvent is called for every successfully parsed and correlated event.
	// conv is the (read-only, non-retained) conversation the event was
	// correlated into; ev is the event just appended (a value copy).
	OnEvent(ctx context.Context, conv *Conversation, ev Event)
}

// EvictionListener is an optional extension a [Listener] may implement.
// When a [Store] evicts a [Conversation] it calls OnEvict for each registered
// listener that implements this interface. Implementations must be fast and
// non-blocking; the Store lock is NOT held during the call.
type EvictionListener interface {
	// OnEvict is called once per evicted conversation, identified by its ID.
	OnEvict(convID string)
}

// -----------------------------------------------------------------------
// Store
// -----------------------------------------------------------------------

// Store maintains the set of active [Conversation]s and implements [proxy.Sink].
//
// It is constructed via [NewStore] and is safe for concurrent use. Memory is
// bounded via an LRU cache with TTL eviction.
type Store struct {
	mu  sync.Mutex
	cfg StoreConfig
	log *slog.Logger
	// list is the LRU doubly-linked list; front = most-recently-used.
	list *list.List
	// lmap maps ConvKey to its list element for O(1) lookup.
	lmap map[ConvKey]*list.Element
	ls   []Listener
	// reg is the optional bot-identity registry used for ToBot resolution.
	// When nil, bot-to-bot recipient resolution is skipped.
	reg *BotRegistry
}

// lruEntry is stored as the Value of each *list.Element.
type lruEntry struct {
	key  ConvKey
	conv *Conversation
}

// NewStore constructs a [Store] with the given configuration, logger, and
// optional event listeners.
//
// logger may be nil (a discard logger is used). listeners are called in order
// for every event; nil entries are skipped.
//
// Pass a non-nil [BotRegistry] via [StoreWithRegistry] to enable bot-to-bot
// recipient resolution.
func NewStore(cfg StoreConfig, logger *slog.Logger, listeners ...Listener) *Store {
	return newStore(cfg, logger, nil, listeners...)
}

// NewStoreWithRegistry constructs a [Store] like [NewStore] but also wires in a
// [BotRegistry] for real bot-to-bot recipient resolution.
func NewStoreWithRegistry(cfg StoreConfig, logger *slog.Logger, reg *BotRegistry, listeners ...Listener) *Store {
	return newStore(cfg, logger, reg, listeners...)
}

func newStore(cfg StoreConfig, logger *slog.Logger, reg *BotRegistry, listeners ...Listener) *Store {
	if cfg.MaxConversations <= 0 {
		cfg.MaxConversations = DefaultMaxConversations
	}
	if cfg.ConvTTL <= 0 {
		cfg.ConvTTL = DefaultConvTTL
	}
	if cfg.LoopWindowSize <= 0 {
		cfg.LoopWindowSize = DefaultLoopWindow
	}
	if cfg.LoopEntryTTL <= 0 {
		cfg.LoopEntryTTL = DefaultLoopTTL
	}
	if logger == nil {
		logger = discardLogger()
	}

	// Filter nil listeners.
	ls := listeners[:0:len(listeners)]
	for _, l := range listeners {
		if l != nil {
			ls = append(ls, l)
		}
	}

	return &Store{
		cfg:  cfg,
		log:  logger,
		list: list.New(),
		lmap: make(map[ConvKey]*list.Element, cfg.MaxConversations),
		ls:   ls,
		reg:  reg,
	}
}

// Record implements [proxy.Sink]. It parses ex, correlates it with an existing
// or new [Conversation], runs loop detection, and notifies all [Listener]s.
//
// For getUpdates responses that contain multiple updates, Record emits one
// [Event] per update so that every hop is individually traced — matching the
// "every hop is traced" guarantee described in the README.
//
// Record is non-blocking: telemetry work is delegated to listeners which must
// themselves be non-blocking.
func (s *Store) Record(ctx context.Context, ex *proxy.Exchange) {
	if ex.Method == "getUpdates" {
		s.recordGetUpdates(ctx, ex)
		return
	}
	if ex.Method == proxy.MethodWebhookIngress {
		s.recordWebhook(ctx, ex)
		return
	}

	pm := parseExchange(ex, s.reg)
	s.recordOne(ctx, pm)
}

// recordGetUpdates handles the batch path for getUpdates: it parses every
// update in the response and calls recordOne for each one that has a usable
// ChatID and FromBot.
func (s *Store) recordGetUpdates(ctx context.Context, ex *proxy.Exchange) {
	pms := parseGetUpdatesAll(ex, s.reg)
	if len(pms) == 0 {
		s.log.Debug("capture: getUpdates response contained no usable updates",
			slog.String("method", ex.Method))
		return
	}
	for _, pm := range pms {
		s.recordOne(ctx, pm)
	}
}

// recordWebhook handles the inbound Telegram webhook path. A webhook delivery
// carries exactly one update in the request body; it is parsed through the
// same per-update resolution as getUpdates so the produced Event is identical
// to the polled equivalent (same From/To, same loop-detection input, same
// conversation correlation).
func (s *Store) recordWebhook(ctx context.Context, ex *proxy.Exchange) {
	pms := parseWebhookUpdate(ex, s.reg)
	if len(pms) == 0 {
		s.log.Debug("capture: webhook delivery contained no usable update",
			slog.String("method", ex.Method))
		return
	}
	for _, pm := range pms {
		s.recordOne(ctx, pm)
	}
}

// recordOne correlates a single [ParsedMessage] into the conversation store,
// runs loop detection, and notifies all registered [Listener]s.
func (s *Store) recordOne(ctx context.Context, pm ParsedMessage) {
	// We need at least a ChatID and FromBot to correlate.
	if pm.ChatID == 0 || pm.FromBot == "" {
		s.log.Debug("capture: skipping exchange with no chat_id or bot token",
			slog.String("method", pm.Method))
		return
	}

	loopKey := loopContentKey(pm.Text, pm.MediaKey)
	now := pm.Timestamp
	if now.IsZero() {
		now = time.Now().UTC()
	}

	s.mu.Lock()

	// Evict TTL-expired conversations before any other work so the LRU cap
	// stays accurate.
	evicted := s.evictStaleLocked(now)

	key := convKeyFor(pm.ChatID, pm.ThreadID, pm.FromBot, pm.ToBot)
	conv, evictedByLRU := s.getOrCreateLocked(key, now)
	evicted = append(evicted, evictedByLRU...)

	// Run loop detection.
	depth, looped := conv.detectLoop(pm.FromBot, pm.ToBot, loopKey, now,
		s.cfg.LoopWindowSize, s.cfg.LoopEntryTTL)
	if looped {
		s.log.Info("capture: loop detected",
			slog.String("conv_id", conv.ID),
			slog.Int("depth", depth),
			slog.String("from", pm.FromBot),
			slog.String("to", pm.ToBot))
	}

	ev := Event{
		ParsedMessage: pm,
		LoopDepth:     depth,
	}
	conv.Events = append(conv.Events, ev)
	conv.LastSeen = now

	// Promote to front of LRU.
	s.list.MoveToFront(s.lmap[key])

	// Copy listeners before releasing lock so we never hold lock during
	// callbacks.
	ls := s.ls

	s.mu.Unlock()

	// Notify eviction listeners outside the lock.
	if len(evicted) > 0 {
		for _, l := range ls {
			if el, ok := l.(EvictionListener); ok {
				for _, id := range evicted {
					el.OnEvict(id)
				}
			}
		}
	}

	// Notify listeners outside the lock.
	for _, l := range ls {
		l.OnEvent(ctx, conv, ev)
	}
}

// ConversationCount returns the current number of active conversations.
func (s *Store) ConversationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list.Len()
}

// -----------------------------------------------------------------------
// LRU helpers (must be called with s.mu held)
// -----------------------------------------------------------------------

// getOrCreateLocked returns the conversation for key, creating it if absent.
// If the LRU is at capacity, the least-recently-used entry is evicted.
// The second return value contains the conversation IDs of any evicted entries.
func (s *Store) getOrCreateLocked(key ConvKey, now time.Time) (*Conversation, []string) {
	if el, ok := s.lmap[key]; ok {
		return lruEntryOf(el).conv, nil
	}

	// Evict LRU tail if at cap.
	var evicted []string
	if s.list.Len() >= s.cfg.MaxConversations {
		evicted = s.evictOldestLocked()
	}

	conv := &Conversation{
		ID:       convID(key),
		Key:      key,
		LastSeen: now,
	}
	el := s.list.PushFront(&lruEntry{key: key, conv: conv})
	s.lmap[key] = el
	return conv, evicted
}

// evictOldestLocked removes the tail (least-recently-used) entry and returns
// its conversation ID.
func (s *Store) evictOldestLocked() []string {
	tail := s.list.Back()
	if tail == nil {
		return nil
	}
	entry := lruEntryOf(tail)
	s.list.Remove(tail)
	delete(s.lmap, entry.key)
	return []string{entry.conv.ID}
}

// evictStaleLocked removes entries that have not been updated within ConvTTL.
// It walks backwards from the LRU tail where stale entries cluster.
// Returns the conversation IDs of all evicted entries.
func (s *Store) evictStaleLocked(now time.Time) []string {
	cutoff := now.Add(-s.cfg.ConvTTL)
	var evicted []string
	for {
		tail := s.list.Back()
		if tail == nil {
			break
		}
		entry := lruEntryOf(tail)
		if entry.conv.LastSeen.After(cutoff) {
			break // tail is fresh; no older entries exist
		}
		evicted = append(evicted, entry.conv.ID)
		s.list.Remove(tail)
		delete(s.lmap, entry.key)
	}
	return evicted
}
