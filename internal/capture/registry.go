package capture

import (
	"sync"
)

// BotRegistry maintains a bidirectional map between Telegram bot user IDs and
// token hashes. It is populated lazily from getMe responses that pass through
// the proxy, and consulted during sendMessage and getUpdates parsing to resolve
// the telegram.bot.to span attribute.
//
// BotRegistry is safe for concurrent use. It is bounded to maxEntries entries;
// oldest registrations are evicted when the cap is exceeded.
type BotRegistry struct {
	mu         sync.RWMutex
	maxEntries int
	// idToHash maps bot Telegram user ID → token hash.
	idToHash map[int64]string
	// hashToID maps token hash → bot Telegram user ID.
	hashToID map[string]int64
	// insertOrder tracks insertion order for eviction (FIFO when at cap).
	insertOrder []int64
}

// defaultRegistryMax is the default maximum number of known bots the registry
// tracks. This is intentionally small: in practice a b2bdbg deployment knows only
// a handful of bots, and the cap prevents unbounded growth in adversarial environments.
const defaultRegistryMax = 1_000

// NewBotRegistry constructs a BotRegistry capped at maxEntries bot identities.
// A value <= 0 uses [defaultRegistryMax].
func NewBotRegistry(maxEntries int) *BotRegistry {
	if maxEntries <= 0 {
		maxEntries = defaultRegistryMax
	}
	return &BotRegistry{
		maxEntries:  maxEntries,
		idToHash:    make(map[int64]string, maxEntries),
		hashToID:    make(map[string]int64, maxEntries),
		insertOrder: make([]int64, 0, maxEntries),
	}
}

// Register records that botID belongs to the bot identified by tokenHash.
//
// If botID is already registered with the same hash, Register is a no-op.
// If botID is already registered with a *different* hash (the bot's token was
// rotated) the mapping is updated in place: the stale reverse entry
// (oldHash → botID) is removed, the new pair installed, and botID keeps its
// original eviction position (no duplicate insertOrder entry, so Snapshot
// cannot return duplicate rows).
// If the registry is at capacity, the oldest entry is evicted first.
func (r *BotRegistry) Register(botID int64, tokenHash string) {
	if botID == 0 || tokenHash == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.idToHash[botID]; ok {
		if existing == tokenHash {
			return // already known, no change
		}
		// Token rotated for a known bot: replace without re-ordering and
		// without leaving the old hash pointing back at this id.
		if r.hashToID[existing] == botID {
			delete(r.hashToID, existing)
		}
		r.idToHash[botID] = tokenHash
		r.hashToID[tokenHash] = botID
		return
	}

	// Evict oldest entry if at cap.
	if len(r.idToHash) >= r.maxEntries && len(r.insertOrder) > 0 {
		oldest := r.insertOrder[0]
		r.insertOrder = r.insertOrder[1:]
		if h, ok := r.idToHash[oldest]; ok {
			delete(r.idToHash, oldest)
			// Only drop the reverse entry if it still points at the evicted
			// id (a prior rotation may have repointed this hash elsewhere).
			if id, ok := r.hashToID[h]; ok && id == oldest {
				delete(r.hashToID, h)
			}
		}
	}

	r.idToHash[botID] = tokenHash
	r.hashToID[tokenHash] = botID
	r.insertOrder = append(r.insertOrder, botID)
}

// HashForID returns the token hash associated with the given Telegram user ID.
// Returns ("", false) if the bot is not in the registry.
func (r *BotRegistry) HashForID(botID int64) (string, bool) {
	if botID == 0 {
		return "", false
	}
	r.mu.RLock()
	h, ok := r.idToHash[botID]
	r.mu.RUnlock()
	return h, ok
}

// IDForHash returns the Telegram user ID associated with the given token hash.
// Returns (0, false) if the bot is not in the registry.
func (r *BotRegistry) IDForHash(tokenHash string) (int64, bool) {
	if tokenHash == "" {
		return 0, false
	}
	r.mu.RLock()
	id, ok := r.hashToID[tokenHash]
	r.mu.RUnlock()
	return id, ok
}

// RegistryEntry is one bot id↔token-hash mapping, as exposed by [Snapshot].
// It deliberately carries only the Telegram bot user ID and the SHA-256
// token-hash prefix that is already written to spans/logs — never a raw token.
type RegistryEntry struct {
	// BotID is the Telegram bot user ID learned from a getMe response.
	BotID int64 `json:"bot_id"`

	// TokenHash is the same short SHA-256 hash already emitted on spans.
	TokenHash string `json:"token_hash"`
}

// Snapshot returns a stable, insertion-ordered copy of every id↔hash mapping
// currently held. It is intended for local-only debug introspection. No raw
// token is ever stored in the registry, so none can be returned here.
func (r *BotRegistry) Snapshot() []RegistryEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]RegistryEntry, 0, len(r.insertOrder))
	for _, id := range r.insertOrder {
		if h, ok := r.idToHash[id]; ok {
			out = append(out, RegistryEntry{BotID: id, TokenHash: h})
		}
	}
	return out
}

// Len returns the current number of entries in the registry.
func (r *BotRegistry) Len() int {
	r.mu.RLock()
	n := len(r.idToHash)
	r.mu.RUnlock()
	return n
}
