package capture

import (
	"fmt"
	"time"
)

// -----------------------------------------------------------------------
// Conversation — one logical trace (trace = one conversation)
// -----------------------------------------------------------------------

// Conversation represents one logical bot↔bot conversation, which maps to a
// single OpenTelemetry trace. It accumulates [Event] records and tracks loop
// detection state.
//
// Conversation is NOT safe for concurrent use by itself; access is serialised
// by the [Store] mutex.
type Conversation struct {
	// ID is the opaque, stable identifier for this conversation (used as OTel
	// trace identifier). It is derived deterministically from the correlation key.
	ID string

	// Key is the correlation triple that identifies this conversation.
	Key ConvKey

	// Events is the ordered list of parsed messages in this conversation.
	Events []Event

	// LastSeen is the time of the most recent event, used for LRU/TTL eviction.
	LastSeen time.Time

	// loopWindow is the sliding window used by the loop detector.
	loopWindow []loopEntry
}

// Event is one message inside a [Conversation].
type Event struct {
	// ParsedMessage contains the parsed Bot API fields.
	ParsedMessage

	// LoopDepth is 0 when no loop is detected for this event.
	// A positive value is the cycle length (how many steps back the repeat
	// was found).
	LoopDepth int
}

// ConvKey is the correlation key that uniquely identifies a conversation.
//
// Per the PLAN: "Trace = one logical conversation (correlation: chat_id + bot
// pair + thread)". All bot activity within the same Telegram chat and thread is
// grouped into one conversation. The participant set (bot pair) is tracked
// inside [Conversation] rather than in the key, because bots join the
// conversation over time as they send or receive messages.
type ConvKey struct {
	// ChatID is the Telegram chat identifier.
	ChatID int64

	// ThreadID is the optional message thread identifier (0 for the main thread).
	ThreadID int64
}

// convKeyFor constructs a [ConvKey] for the given chat and thread.
// fromBot and toBot are accepted for signature compatibility but are tracked
// inside the Conversation, not in the key itself.
func convKeyFor(chatID, threadID int64, _, _ string) ConvKey {
	return ConvKey{
		ChatID:   chatID,
		ThreadID: threadID,
	}
}

// convID derives a stable, human-readable identifier from the key.
func convID(k ConvKey) string {
	return fmt.Sprintf("chat%d_th%d", k.ChatID, k.ThreadID)
}

// -----------------------------------------------------------------------
// Loop detection
// -----------------------------------------------------------------------

// loopEntry is a single entry in the per-conversation loop-detection sliding
// window.
//
// contentKey is the deterministic loop signal: it is derived from the message
// text hash and, when the message carries no text, from a stable media
// identity (file_unique_id / file_id) so repeated media/file hand-offs are
// detected, not only repeated text. An empty contentKey means the message
// carried neither text nor recognised media and is skipped by the detector
// (preserving the original text-only behaviour for text messages).
type loopEntry struct {
	from       string
	to         string
	contentKey string
	seenAt     time.Time
}

// DefaultLoopWindow is the number of recent events examined when looking for
// repeating (from,to,text-hash) cycles.
const DefaultLoopWindow = 20

// DefaultLoopTTL is how long a loop-window entry is retained. Entries older
// than this are pruned before the search, preventing a brief pause from
// masking a later loop.
const DefaultLoopTTL = 5 * time.Minute

// loopContentKey derives the deterministic loop-detection signal from a
// message's text hash and media identity.
//
//   - Text messages key on the text hash (unchanged from the original
//     text-only behaviour, so existing tests and traces are preserved).
//   - Non-text messages (no text/caption) key on a stable media identity,
//     prefixed so a media key can never collide with a text hash.
//   - Messages with neither return "" and are skipped by the detector.
func loopContentKey(text, mediaKey string) string {
	if th := textHash(text); th != "" {
		return th
	}
	if mediaKey != "" {
		return "m:" + mediaKey
	}
	return ""
}

// detectLoop checks whether the incoming (from, to, contentKey) tuple matches a
// prior entry within the conversation's sliding window. contentKey is the
// text-hash-or-media-identity signal produced by [loopContentKey].
//
// If a match is found, detectLoop returns the cycle depth (distance back to
// the matching entry, 1-indexed) and true. Otherwise it returns 0, false.
//
// The entry is always appended to the window after the check, and entries
// older than ttl are pruned.
func (c *Conversation) detectLoop(from, to, contentKey string, now time.Time, maxWindow int, ttl time.Duration) (depth int, detected bool) {
	// Prune stale entries first.
	cutoff := now.Add(-ttl)
	keep := c.loopWindow[:0]
	for _, e := range c.loopWindow {
		if e.seenAt.After(cutoff) {
			keep = append(keep, e)
		}
	}
	c.loopWindow = keep

	// Enforce window size cap.
	if maxWindow <= 0 {
		maxWindow = DefaultLoopWindow
	}

	// Search backwards for the same (from, to, contentKey).
	if contentKey != "" { // only check when there is a meaningful signal
		for i := len(c.loopWindow) - 1; i >= 0; i-- {
			e := c.loopWindow[i]
			if e.from == from && e.to == to && e.contentKey == contentKey {
				depth = len(c.loopWindow) - i
				detected = true
				break
			}
		}
	}

	// Append current entry, capping total window size.
	c.loopWindow = append(c.loopWindow, loopEntry{from: from, to: to, contentKey: contentKey, seenAt: now})
	if len(c.loopWindow) > maxWindow {
		c.loopWindow = c.loopWindow[len(c.loopWindow)-maxWindow:]
	}

	return depth, detected
}
