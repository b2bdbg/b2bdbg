// Package capture parses Telegram Bot API method calls into a Conversation model,
// correlates bot ↔ bot exchanges, and detects infinite-loop patterns.
//
// # Conversation model
//
// A Conversation represents one logical trace: a coherent exchange inside a
// single Telegram chat (and optional message thread). The correlation key is
// (chat_id, thread_id) — see convKeyFor in conversation.go. The bot pair is
// recorded on each Event but is *not* part of the key, so a thread that
// involves multiple bots (router → specialist → router → human-approval)
// collects into one Conversation and one trace.
//
// # Loop detection
//
// When the same (from, to, text-hash) tuple recurs within a configurable time
// window the offending event is flagged with a positive LoopDepth and the
// b2b_loops_total counter is incremented via telemetry.Sink.
//
// # Memory bounds
//
// The store uses an LRU cache with TTL eviction so that stale conversations
// are pruned automatically. Neither the LRU map nor any per-conversation state
// can grow without bound.
//
// # Concurrency
//
// All exported methods on Store are safe for concurrent use.
package capture
