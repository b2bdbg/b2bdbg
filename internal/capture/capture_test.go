package capture_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// sendMessageExchange builds a proxy.Exchange that looks like a sendMessage call.
func sendMessageExchange(tokenHash string, chatID int64, text string, msgID int64) *proxy.Exchange {
	req, _ := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
	resp, _ := json.Marshal(map[string]any{
		"ok": true,
		"result": map[string]any{
			"message_id": msgID,
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"text":       text,
			"date":       1700000000,
		},
	})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  tokenHash,
		Method:     "sendMessage",
		ReqBody:    req,
		RespBody:   resp,
		StatusCode: 200,
		Duration:   10 * time.Millisecond,
	}
}

// getUpdatesExchange builds a proxy.Exchange that looks like a getUpdates call
// returning one message.
func getUpdatesExchange(tokenHash string, chatID int64, text string, msgID int64) *proxy.Exchange {
	resp, _ := json.Marshal(map[string]any{
		"ok": true,
		"result": []any{
			map[string]any{
				"update_id": 100000001,
				"message": map[string]any{
					"message_id": msgID,
					"chat":       map[string]any{"id": chatID, "type": "private"},
					"text":       text,
					"date":       1700000001,
				},
			},
		},
	})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  tokenHash,
		Method:     "getUpdates",
		RespBody:   resp,
		StatusCode: 200,
		Duration:   10 * time.Millisecond,
	}
}

// -----------------------------------------------------------------------
// recordingListener collects events for assertions
// -----------------------------------------------------------------------

type recordingListener struct {
	events []capture.Event
	convs  []*capture.Conversation
}

func (r *recordingListener) OnEvent(_ context.Context, conv *capture.Conversation, ev capture.Event) {
	r.events = append(r.events, ev)
	r.convs = append(r.convs, conv)
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

// TestParseSendMessage verifies that a sendMessage Exchange is correctly parsed.
func TestParseSendMessage(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{}, nil, rec)

	ex := sendMessageExchange("hash_botA", 42, "hello world", 99)
	store.Record(context.Background(), ex)

	if len(rec.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(rec.events))
	}

	ev := rec.events[0]
	pm := ev.ParsedMessage

	if pm.Method != "sendMessage" {
		t.Errorf("Method = %q, want sendMessage", pm.Method)
	}
	if pm.ChatID != 42 {
		t.Errorf("ChatID = %d, want 42", pm.ChatID)
	}
	if pm.Text != "hello world" {
		t.Errorf("Text = %q, want hello world", pm.Text)
	}
	if pm.TextLen != len("hello world") {
		t.Errorf("TextLen = %d, want %d", pm.TextLen, len("hello world"))
	}
	if pm.MessageID != 99 {
		t.Errorf("MessageID = %d, want 99", pm.MessageID)
	}
	if pm.FromBot != "hash_botA" {
		t.Errorf("FromBot = %q, want hash_botA", pm.FromBot)
	}
}

// TestParseGetUpdates verifies getUpdates response parsing.
func TestParseGetUpdates(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{}, nil, rec)

	ex := getUpdatesExchange("hash_botB", 55, "ping", 7)
	store.Record(context.Background(), ex)

	if len(rec.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(rec.events))
	}
	pm := rec.events[0].ParsedMessage
	if pm.ChatID != 55 {
		t.Errorf("ChatID = %d, want 55", pm.ChatID)
	}
	if pm.MessageID != 7 {
		t.Errorf("MessageID = %d, want 7", pm.MessageID)
	}
	if pm.Text != "ping" {
		t.Errorf("Text = %q, want ping", pm.Text)
	}
}

// TestConversationCorrelation checks that multiple exchanges in the same chat —
// even from different bots — land in the same conversation. Per the PLAN,
// the correlation key is (chat_id + thread), so all bot activity in a chat
// forms one logical trace.
func TestConversationCorrelation(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{}, nil, rec)
	ctx := context.Background()

	chatID := int64(1001)
	botA := "aaaa1111"
	botB := "bbbb2222"

	// Bot A sends a message.
	store.Record(ctx, sendMessageExchange(botA, chatID, "hi", 1))
	// Bot B sends a reply in the same chat.
	store.Record(ctx, sendMessageExchange(botB, chatID, "hello back", 2))
	// Bot A sends again.
	store.Record(ctx, sendMessageExchange(botA, chatID, "great", 3))

	if len(rec.events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(rec.events))
	}

	// All events should reference the same conversation because they share
	// chat_id + thread_id (the correlation key).
	firstConvID := rec.convs[0].ID
	for i, c := range rec.convs {
		if c.ID != firstConvID {
			t.Errorf("event %d: conv.ID = %q, want %q (same chat must map to same conv)", i, c.ID, firstConvID)
		}
	}

	// The conversation should have accumulated all 3 events.
	if len(rec.convs[2].Events) != 3 {
		t.Errorf("conv.Events count = %d, want 3", len(rec.convs[2].Events))
	}
}

// TestDifferentChatsAreDifferentConversations confirms that different chat IDs
// produce different conversations.
func TestDifferentChatsAreDifferentConversations(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{}, nil, rec)
	ctx := context.Background()

	store.Record(ctx, sendMessageExchange("bot1", 1001, "msg", 1))
	store.Record(ctx, sendMessageExchange("bot1", 2002, "msg", 2))

	if len(rec.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(rec.events))
	}

	if rec.convs[0].ID == rec.convs[1].ID {
		t.Errorf("expected different conversation IDs for different chats")
	}
}

// TestLoopDetection verifies that a repeating (from,to,text-hash) cycle is
// detected with the expected depth.
func TestLoopDetection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		exchanges  []*proxy.Exchange
		wantDepths []int // expected LoopDepth for each event in order
	}{
		{
			name: "no loop when messages differ",
			exchanges: func() []*proxy.Exchange {
				return []*proxy.Exchange{
					sendMessageExchange("aa", 1, "hello", 1),
					sendMessageExchange("aa", 1, "world", 2),
					sendMessageExchange("aa", 1, "foo", 3),
				}
			}(),
			wantDepths: []int{0, 0, 0},
		},
		{
			name: "loop detected at depth 1 for immediate repeat",
			exchanges: func() []*proxy.Exchange {
				return []*proxy.Exchange{
					sendMessageExchange("aa", 1, "ping", 1),
					sendMessageExchange("aa", 1, "ping", 2), // same from+to+text
				}
			}(),
			wantDepths: []int{0, 1},
		},
		{
			name: "loop detected across gap",
			exchanges: func() []*proxy.Exchange {
				return []*proxy.Exchange{
					sendMessageExchange("aa", 1, "ping", 1),
					sendMessageExchange("aa", 1, "other", 2),
					sendMessageExchange("aa", 1, "ping", 3), // matches entry 0, depth=2
				}
			}(),
			wantDepths: []int{0, 0, 2},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := &recordingListener{}
			store := capture.NewStore(capture.StoreConfig{
				LoopWindowSize: 20,
				LoopEntryTTL:   10 * time.Minute,
			}, nil, rec)

			for _, ex := range tc.exchanges {
				store.Record(context.Background(), ex)
			}

			if len(rec.events) != len(tc.wantDepths) {
				t.Fatalf("event count = %d, want %d", len(rec.events), len(tc.wantDepths))
			}
			for i, ev := range rec.events {
				if ev.LoopDepth != tc.wantDepths[i] {
					t.Errorf("event[%d].LoopDepth = %d, want %d", i, ev.LoopDepth, tc.wantDepths[i])
				}
			}
		})
	}
}

// TestLoopTTLEviction checks that loop-window entries older than LoopEntryTTL
// are not matched, preventing false positives after a long pause.
func TestLoopTTLEviction(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{
		LoopWindowSize: 20,
		LoopEntryTTL:   1 * time.Millisecond, // very short TTL
	}, nil, rec)

	ctx := context.Background()

	// First message.
	ex1 := sendMessageExchange("aa", 1, "ping", 1)
	ex1.Timestamp = time.Now().UTC().Add(-1 * time.Hour) // old
	store.Record(ctx, ex1)

	// Same text but the prior entry should have expired.
	ex2 := sendMessageExchange("aa", 1, "ping", 2)
	ex2.Timestamp = time.Now().UTC()
	store.Record(ctx, ex2)

	if len(rec.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(rec.events))
	}
	if rec.events[1].LoopDepth != 0 {
		t.Errorf("expected no loop after TTL eviction, got depth %d", rec.events[1].LoopDepth)
	}
}

// TestLRUEviction verifies that when MaxConversations is exceeded, the
// least-recently-used conversation is evicted and the total count stays bounded.
func TestLRUEviction(t *testing.T) {
	t.Parallel()

	const maxConvs = 5
	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{
		MaxConversations: maxConvs,
		ConvTTL:          1 * time.Hour, // disable TTL eviction
	}, nil, rec)

	ctx := context.Background()

	// Insert maxConvs+2 distinct conversations (distinct chat IDs).
	for i := int64(0); i < int64(maxConvs+2); i++ {
		store.Record(ctx, sendMessageExchange("bot1", i, "hi", i))
	}

	count := store.ConversationCount()
	if count > maxConvs {
		t.Errorf("ConversationCount = %d, want <= %d (LRU cap)", count, maxConvs)
	}
}

// TestConcurrentRecord verifies that concurrent calls to Record do not race.
func TestConcurrentRecord(t *testing.T) {
	t.Parallel()

	store := capture.NewStore(capture.StoreConfig{}, nil)
	ctx := context.Background()

	done := make(chan struct{})
	for g := 0; g < 20; g++ {
		go func(g int) {
			for i := 0; i < 50; i++ {
				store.Record(ctx, sendMessageExchange("bot1", int64(g), "msg", int64(i)))
			}
			done <- struct{}{}
		}(g)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// TestSkipsExchangeWithNoChatID verifies that exchanges with no parseable
// chat_id are silently dropped.
func TestSkipsExchangeWithNoChatID(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{}, nil, rec)

	ex := &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  "abc123",
		Method:     "sendMessage",
		ReqBody:    []byte(`{"text":"hello"}`), // no chat_id
		RespBody:   []byte(`{"ok":true}`),
		StatusCode: 200,
	}
	store.Record(context.Background(), ex)

	if len(rec.events) != 0 {
		t.Errorf("expected 0 events for exchange with no chat_id, got %d", len(rec.events))
	}
}

// getMeExchange builds a proxy.Exchange that looks like a getMe response.
func getMeExchange(tokenHash string, botUserID int64) *proxy.Exchange {
	resp, _ := json.Marshal(map[string]any{
		"ok": true,
		"result": map[string]any{
			"id":         botUserID,
			"is_bot":     true,
			"first_name": "TestBot",
		},
	})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  tokenHash,
		Method:     "getMe",
		RespBody:   resp,
		StatusCode: 200,
		Duration:   5 * time.Millisecond,
	}
}

// getUpdatesExchangeWithSender builds a getUpdates Exchange where the message
// sender is a known bot (has IsBot=true, From.ID set).
func getUpdatesExchangeWithSender(receiverHash string, chatID int64, senderUserID int64, text string, msgID int64) *proxy.Exchange {
	resp, _ := json.Marshal(map[string]any{
		"ok": true,
		"result": []any{
			map[string]any{
				"update_id": 100000002,
				"message": map[string]any{
					"message_id": msgID,
					"chat":       map[string]any{"id": chatID, "type": "private"},
					"from": map[string]any{
						"id":         senderUserID,
						"is_bot":     true,
						"first_name": "SenderBot",
					},
					"text": text,
					"date": 1700000002,
				},
			},
		},
	})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  receiverHash,
		Method:     "getUpdates",
		RespBody:   resp,
		StatusCode: 200,
		Duration:   10 * time.Millisecond,
	}
}

// -----------------------------------------------------------------------
// Bot registry tests
// -----------------------------------------------------------------------

// TestBotRegistryRegisterAndLookup verifies the basic register / lookup
// contract of BotRegistry.
func TestBotRegistryRegisterAndLookup(t *testing.T) {
	t.Parallel()

	reg := capture.NewBotRegistry(0)

	// Nothing registered yet.
	if _, ok := reg.HashForID(42); ok {
		t.Error("expected no result for unregistered bot ID")
	}

	reg.Register(42, "hashA")

	hash, ok := reg.HashForID(42)
	if !ok {
		t.Fatal("expected hash after Register")
	}
	if hash != "hashA" {
		t.Errorf("HashForID(42) = %q, want hashA", hash)
	}

	id, ok := reg.IDForHash("hashA")
	if !ok {
		t.Fatal("expected id via IDForHash")
	}
	if id != 42 {
		t.Errorf("IDForHash(hashA) = %d, want 42", id)
	}
}

// TestBotRegistryTokenRotation verifies that re-registering an existing botID
// with a new hash replaces the mapping cleanly: the old reverse entry
// (oldHash → botID) is dropped, the new pair is installed, and Snapshot
// reports the bot exactly once (no duplicate insertOrder entry).
func TestBotRegistryTokenRotation(t *testing.T) {
	t.Parallel()

	reg := capture.NewBotRegistry(0)
	reg.Register(7, "oldHash")
	reg.Register(7, "newHash")

	if h, ok := reg.HashForID(7); !ok || h != "newHash" {
		t.Errorf("HashForID(7) = (%q, %v); want (newHash, true)", h, ok)
	}
	if id, ok := reg.IDForHash("newHash"); !ok || id != 7 {
		t.Errorf("IDForHash(newHash) = (%d, %v); want (7, true)", id, ok)
	}
	if id, ok := reg.IDForHash("oldHash"); ok {
		t.Errorf("IDForHash(oldHash) still resolves to %d; stale reverse entry not cleared", id)
	}
	snap := reg.Snapshot()
	if len(snap) != 1 {
		t.Errorf("Snapshot len = %d; want 1 (duplicate insertOrder entry?)", len(snap))
	}
	if reg.Len() != 1 {
		t.Errorf("Len() = %d; want 1", reg.Len())
	}
}

// TestBotRegistryCapEviction verifies that the oldest entry is evicted when
// the registry exceeds its maximum size.
func TestBotRegistryCapEviction(t *testing.T) {
	t.Parallel()

	const maxSize = 3
	reg := capture.NewBotRegistry(maxSize)

	// Fill to the maximum.
	reg.Register(1, "h1")
	reg.Register(2, "h2")
	reg.Register(3, "h3")
	if reg.Len() != maxSize {
		t.Fatalf("Len = %d, want %d", reg.Len(), maxSize)
	}

	// Insert a 4th entry — should evict bot ID 1 (oldest).
	reg.Register(4, "h4")
	if reg.Len() != maxSize {
		t.Fatalf("Len = %d after eviction, want %d", reg.Len(), maxSize)
	}

	// Bot 1 should be gone.
	if _, ok := reg.HashForID(1); ok {
		t.Error("expected bot 1 to be evicted")
	}
	// Bot 4 must be present.
	if _, ok := reg.HashForID(4); !ok {
		t.Error("expected bot 4 to be present")
	}
}

// TestToBotResolvedViaSendMessage checks that when bot B's user ID is known
// (from a prior getMe) and bot A sends a message with chat_id = bot B's ID,
// the ToBot field is set to bot B's token hash.
func TestToBotResolvedViaSendMessage(t *testing.T) {
	t.Parallel()

	reg := capture.NewBotRegistry(0)
	rec := &recordingListener{}
	store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
	ctx := context.Background()

	const (
		botAHash   = "hashBotA"
		botBHash   = "hashBotB"
		botBUserID = int64(9999)
		chatID     = int64(9999) // sending to bot B's private chat = same as bot B user ID
	)

	// Step 1: bot B calls getMe — registry learns botBUserID → botBHash.
	store.Record(ctx, getMeExchange(botBHash, botBUserID))

	// Step 2: bot A sends a message with chat_id == botBUserID.
	req, _ := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    "hello bot B",
	})
	resp, _ := json.Marshal(map[string]any{
		"ok": true,
		"result": map[string]any{
			"message_id": int64(1),
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"text":       "hello bot B",
			"date":       1700000010,
		},
	})
	sendEx := &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  botAHash,
		Method:     "sendMessage",
		ReqBody:    req,
		RespBody:   resp,
		StatusCode: 200,
		Duration:   5 * time.Millisecond,
	}
	store.Record(ctx, sendEx)

	// Find the sendMessage event.
	var sendEvent *capture.Event
	for i, ev := range rec.events {
		if ev.Method == "sendMessage" {
			e := rec.events[i]
			sendEvent = &e
			break
		}
	}
	if sendEvent == nil {
		t.Fatal("no sendMessage event found")
	}

	if sendEvent.ToBot != botBHash {
		t.Errorf("ToBot = %q, want %q (bot B's hash)", sendEvent.ToBot, botBHash)
	}
	if sendEvent.FromBot != botAHash {
		t.Errorf("FromBot = %q, want %q (bot A's hash)", sendEvent.FromBot, botAHash)
	}
}

// TestABEdgeViaGetUpdates checks that when bot A's user ID is known and bot B
// receives a getUpdates response containing a message from bot A, the parsed
// event has FromBot=botA, ToBot=botB — establishing the A→B edge.
func TestABEdgeViaGetUpdates(t *testing.T) {
	t.Parallel()

	reg := capture.NewBotRegistry(0)
	rec := &recordingListener{}
	store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
	ctx := context.Background()

	const (
		botAHash   = "hashBotA"
		botBHash   = "hashBotB"
		botAUserID = int64(1111)
		chatID     = int64(7777)
	)

	// Bot A registers itself via getMe.
	store.Record(ctx, getMeExchange(botAHash, botAUserID))

	// Bot B polls getUpdates and gets a message sent by bot A.
	store.Record(ctx, getUpdatesExchangeWithSender(botBHash, chatID, botAUserID, "task for B", 5))

	// Find the getUpdates event.
	var updEvent *capture.Event
	for i, ev := range rec.events {
		if ev.Method == "getUpdates" {
			e := rec.events[i]
			updEvent = &e
			break
		}
	}
	if updEvent == nil {
		t.Fatal("no getUpdates event found")
	}

	if updEvent.FromBot != botAHash {
		t.Errorf("FromBot = %q, want %q (sender = bot A)", updEvent.FromBot, botAHash)
	}
	if updEvent.ToBot != botBHash {
		t.Errorf("ToBot = %q, want %q (receiver = bot B)", updEvent.ToBot, botBHash)
	}
}

// -----------------------------------------------------------------------
// getUpdates batch-parsing tests
// -----------------------------------------------------------------------

// getUpdatesMultiExchange builds a proxy.Exchange whose getUpdates response
// contains multiple updates.  Each entry in msgs is one update: (chatID,
// senderUserID — 0 means no From field, isBot, text, msgID).
type batchMsg struct {
	chatID       int64
	senderUserID int64
	isBot        bool
	text         string
	msgID        int64
}

func getUpdatesMultiExchange(receiverHash string, msgs []batchMsg) *proxy.Exchange {
	updates := make([]any, 0, len(msgs))
	for i, m := range msgs {
		msg := map[string]any{
			"message_id": m.msgID,
			"chat":       map[string]any{"id": m.chatID, "type": "private"},
			"text":       m.text,
			"date":       1700000000 + i,
		}
		if m.senderUserID != 0 {
			msg["from"] = map[string]any{
				"id":         m.senderUserID,
				"is_bot":     m.isBot,
				"first_name": "Sender",
			}
		}
		updates = append(updates, map[string]any{
			"update_id": 200000000 + i,
			"message":   msg,
		})
	}
	resp, _ := json.Marshal(map[string]any{
		"ok":     true,
		"result": updates,
	})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  receiverHash,
		Method:     "getUpdates",
		RespBody:   resp,
		StatusCode: 200,
		Duration:   15 * time.Millisecond,
	}
}

// TestGetUpdatesBatchProducesOneEventPerUpdate verifies that a getUpdates
// response containing N updates produces exactly N capture Events (one per
// message), each with the correct per-message ChatID, MessageID, From/To
// resolution, and text.  It also verifies that loop detection is applied
// independently per message across the batch.
func TestGetUpdatesBatchProducesOneEventPerUpdate(t *testing.T) {
	t.Parallel()

	const (
		receiverHash = "hashReceiver"
		botAHash     = "hashBotA"
		botAUserID   = int64(1001)
		chatID1      = int64(5001)
		chatID2      = int64(5002)
		chatID3      = int64(5003)
	)

	type testCase struct {
		name           string
		setup          func(reg *capture.BotRegistry)
		msgs           []batchMsg
		wantEventCount int
		// per-event assertions: index → check function
		checkEvents func(t *testing.T, events []capture.Event)
	}

	cases := []testCase{
		{
			name:  "three updates — one from known bot, two from human",
			setup: func(reg *capture.BotRegistry) { reg.Register(botAUserID, botAHash) },
			msgs: []batchMsg{
				// update 0: from known bot A
				{chatID: chatID1, senderUserID: botAUserID, isBot: true, text: "task", msgID: 10},
				// update 1: from a human (unknown sender)
				{chatID: chatID2, senderUserID: 9999, isBot: false, text: "hello", msgID: 11},
				// update 2: from another human — no From at all
				{chatID: chatID3, senderUserID: 0, isBot: false, text: "ping", msgID: 12},
			},
			wantEventCount: 3,
			checkEvents: func(t *testing.T, events []capture.Event) {
				t.Helper()
				// event[0]: bot A → receiver
				if events[0].FromBot != botAHash {
					t.Errorf("[0] FromBot = %q, want %q (known bot sender)", events[0].FromBot, botAHash)
				}
				if events[0].ToBot != receiverHash {
					t.Errorf("[0] ToBot = %q, want %q (polling receiver)", events[0].ToBot, receiverHash)
				}
				if events[0].ChatID != chatID1 {
					t.Errorf("[0] ChatID = %d, want %d", events[0].ChatID, chatID1)
				}
				if events[0].MessageID != 10 {
					t.Errorf("[0] MessageID = %d, want 10", events[0].MessageID)
				}
				if events[0].Text != "task" {
					t.Errorf("[0] Text = %q, want task", events[0].Text)
				}

				// event[1]: human sender — FromBot stays as receiver (no swap)
				if events[1].FromBot != receiverHash {
					t.Errorf("[1] FromBot = %q, want %q (no known bot → stays receiver)", events[1].FromBot, receiverHash)
				}
				if events[1].ToBot != "" {
					t.Errorf("[1] ToBot = %q, want empty (human sender)", events[1].ToBot)
				}
				if events[1].ChatID != chatID2 {
					t.Errorf("[1] ChatID = %d, want %d", events[1].ChatID, chatID2)
				}

				// event[2]: no From field — FromBot stays as receiver
				if events[2].FromBot != receiverHash {
					t.Errorf("[2] FromBot = %q, want %q (no From → stays receiver)", events[2].FromBot, receiverHash)
				}
				if events[2].ChatID != chatID3 {
					t.Errorf("[2] ChatID = %d, want %d", events[2].ChatID, chatID3)
				}
			},
		},
		{
			name:  "N=1 single update behaves identically to old path",
			setup: func(_ *capture.BotRegistry) {},
			msgs: []batchMsg{
				{chatID: chatID1, senderUserID: 0, isBot: false, text: "solo", msgID: 20},
			},
			wantEventCount: 1,
			checkEvents: func(t *testing.T, events []capture.Event) {
				t.Helper()
				if events[0].Text != "solo" {
					t.Errorf("[0] Text = %q, want solo", events[0].Text)
				}
				if events[0].ChatID != chatID1 {
					t.Errorf("[0] ChatID = %d, want %d", events[0].ChatID, chatID1)
				}
			},
		},
		{
			name:  "loop detection fires per-message within batch",
			setup: func(_ *capture.BotRegistry) {},
			msgs: []batchMsg{
				// Two updates in the same chat with the same text — should
				// trigger a loop on the second one.
				{chatID: chatID1, senderUserID: 0, isBot: false, text: "ping", msgID: 30},
				{chatID: chatID1, senderUserID: 0, isBot: false, text: "ping", msgID: 31},
			},
			wantEventCount: 2,
			checkEvents: func(t *testing.T, events []capture.Event) {
				t.Helper()
				if events[0].LoopDepth != 0 {
					t.Errorf("[0] LoopDepth = %d, want 0 (first occurrence)", events[0].LoopDepth)
				}
				if events[1].LoopDepth == 0 {
					t.Errorf("[1] LoopDepth = 0, want > 0 (same from+to+text should loop)")
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := capture.NewBotRegistry(0)
			if tc.setup != nil {
				tc.setup(reg)
			}
			rec := &recordingListener{}
			store := capture.NewStoreWithRegistry(capture.StoreConfig{
				LoopWindowSize: 20,
				LoopEntryTTL:   10 * time.Minute,
			}, nil, reg, rec)

			ex := getUpdatesMultiExchange(receiverHash, tc.msgs)
			store.Record(context.Background(), ex)

			if len(rec.events) != tc.wantEventCount {
				t.Fatalf("event count = %d, want %d", len(rec.events), tc.wantEventCount)
			}
			if tc.checkEvents != nil {
				tc.checkEvents(t, rec.events)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Eviction listener test
// -----------------------------------------------------------------------

// evictionRecorder collects conversation IDs that were evicted.
type evictionRecorder struct {
	recordingListener
	evicted []string
}

func (r *evictionRecorder) OnEvict(convID string) {
	r.evicted = append(r.evicted, convID)
}

// TestStoreEvictionCallbackFired verifies that when the LRU cap is exceeded,
// OnEvict is called for the displaced conversation with its correct ID.
func TestStoreEvictionCallbackFired(t *testing.T) {
	t.Parallel()

	const maxConvs = 3
	rec := &evictionRecorder{}
	store := capture.NewStore(capture.StoreConfig{
		MaxConversations: maxConvs,
		ConvTTL:          1 * time.Hour,
	}, nil, rec)
	ctx := context.Background()

	// Insert maxConvs conversations. Use chat IDs starting at 1 to avoid 0
	// which the store skips (ChatID==0 is treated as unparseable).
	for i := int64(1); i <= maxConvs; i++ {
		store.Record(ctx, sendMessageExchange("bot1", i, "hi", i))
	}
	if len(rec.evicted) != 0 {
		t.Fatalf("expected 0 evictions while filling to cap, got %d", len(rec.evicted))
	}

	// Insert one more distinct conversation — should evict the oldest (chat 1).
	store.Record(ctx, sendMessageExchange("bot1", int64(maxConvs+1), "hi", int64(maxConvs+1)))
	if len(rec.evicted) != 1 {
		t.Fatalf("expected 1 eviction after exceeding cap, got %d", len(rec.evicted))
	}

	// Total store count must not exceed the cap.
	if count := store.ConversationCount(); count > maxConvs {
		t.Errorf("ConversationCount = %d, want <= %d", count, maxConvs)
	}
}

// -----------------------------------------------------------------------
// Webhook ingestion parity tests
// -----------------------------------------------------------------------

// webhookExchange builds a proxy.Exchange that looks like an inbound Telegram
// webhook delivery: a single Update in the request body, with TokenHash set to
// the receiving bot's hash (the route's configured bot — the polling-bot
// analogue).
func webhookExchange(receiverHash string, chatID, senderUserID int64, isBot bool, text string, msgID int64) *proxy.Exchange {
	msg := map[string]any{
		"message_id": msgID,
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"text":       text,
		"date":       1700000002,
	}
	if senderUserID != 0 {
		msg["from"] = map[string]any{
			"id":         senderUserID,
			"is_bot":     isBot,
			"first_name": "Sender",
		}
	}
	body, _ := json.Marshal(map[string]any{
		"update_id": 900000010,
		"message":   msg,
	})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  receiverHash,
		Method:     proxy.MethodWebhookIngress,
		ReqBody:    body,
		StatusCode: 200,
		Duration:   12 * time.Millisecond,
	}
}

// TestWebhookABEdgeMatchesGetUpdates is the core parity assertion: a webhook
// update from a known bot A delivered to bot B's route MUST produce the same
// A→B edge that the polled getUpdates equivalent produces.
func TestWebhookABEdgeMatchesGetUpdates(t *testing.T) {
	t.Parallel()

	const (
		botAHash   = "hashBotA"
		botBHash   = "hashBotB"
		botAUserID = int64(1111)
		chatID     = int64(7777)
		text       = "task for B"
		msgID      = int64(5)
	)

	// --- Poll path ---------------------------------------------------------
	regP := capture.NewBotRegistry(0)
	recP := &recordingListener{}
	storeP := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, regP, recP)
	storeP.Record(context.Background(), getMeExchange(botAHash, botAUserID))
	storeP.Record(context.Background(), getUpdatesExchangeWithSender(botBHash, chatID, botAUserID, text, msgID))

	// --- Webhook path ------------------------------------------------------
	regW := capture.NewBotRegistry(0)
	recW := &recordingListener{}
	storeW := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, regW, recW)
	storeW.Record(context.Background(), getMeExchange(botAHash, botAUserID))
	storeW.Record(context.Background(), webhookExchange(botBHash, chatID, botAUserID, true, text, msgID))

	findInbound := func(evs []capture.Event) *capture.Event {
		for i := range evs {
			if evs[i].Method == "getUpdates" || evs[i].Method == proxy.MethodWebhookIngress {
				return &evs[i]
			}
		}
		return nil
	}

	pe := findInbound(recP.events)
	we := findInbound(recW.events)
	if pe == nil {
		t.Fatal("poll path produced no inbound event")
	}
	if we == nil {
		t.Fatal("webhook path produced no inbound event")
	}

	// The A→B edge must be identical.
	if pe.FromBot != botAHash || pe.ToBot != botBHash {
		t.Fatalf("poll edge wrong: From=%q To=%q", pe.FromBot, pe.ToBot)
	}
	if we.FromBot != pe.FromBot {
		t.Errorf("FromBot: webhook=%q poll=%q (must match)", we.FromBot, pe.FromBot)
	}
	if we.ToBot != pe.ToBot {
		t.Errorf("ToBot: webhook=%q poll=%q (must match)", we.ToBot, pe.ToBot)
	}
	if we.ChatID != pe.ChatID {
		t.Errorf("ChatID: webhook=%d poll=%d (must match)", we.ChatID, pe.ChatID)
	}
	if we.MessageID != pe.MessageID {
		t.Errorf("MessageID: webhook=%d poll=%d (must match)", we.MessageID, pe.MessageID)
	}
	if we.Text != pe.Text {
		t.Errorf("Text: webhook=%q poll=%q (must match)", we.Text, pe.Text)
	}

	// Conversation correlation must land on the same conversation ID so both
	// modes feed the same OTel trace.
	if recW.convs[len(recW.convs)-1].ID != recP.convs[len(recP.convs)-1].ID {
		t.Errorf("conversation ID: webhook=%q poll=%q (must match)",
			recW.convs[len(recW.convs)-1].ID, recP.convs[len(recP.convs)-1].ID)
	}
}

// TestWebhookLoopDetectionParity verifies that loop detection on a repeated
// webhook (from,to,text) triggers identically to the polled equivalent.
func TestWebhookLoopDetectionParity(t *testing.T) {
	t.Parallel()

	const (
		botAHash   = "hashLoopA"
		botBHash   = "hashLoopB"
		botAUserID = int64(2222)
		chatID     = int64(8888)
		text       = "repeat me"
	)

	loopDepth := func(rec *recordingListener) int {
		maxDepth := 0
		for _, ev := range rec.events {
			if ev.LoopDepth > maxDepth {
				maxDepth = ev.LoopDepth
			}
		}
		return maxDepth
	}

	// Poll: same message arrives twice via getUpdates.
	regP := capture.NewBotRegistry(0)
	recP := &recordingListener{}
	storeP := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, regP, recP)
	storeP.Record(context.Background(), getMeExchange(botAHash, botAUserID))
	storeP.Record(context.Background(), getUpdatesExchangeWithSender(botBHash, chatID, botAUserID, text, 1))
	storeP.Record(context.Background(), getUpdatesExchangeWithSender(botBHash, chatID, botAUserID, text, 2))

	// Webhook: same message arrives twice via webhook.
	regW := capture.NewBotRegistry(0)
	recW := &recordingListener{}
	storeW := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, regW, recW)
	storeW.Record(context.Background(), getMeExchange(botAHash, botAUserID))
	storeW.Record(context.Background(), webhookExchange(botBHash, chatID, botAUserID, true, text, 1))
	storeW.Record(context.Background(), webhookExchange(botBHash, chatID, botAUserID, true, text, 2))

	if got := loopDepth(recW); got != loopDepth(recP) {
		t.Errorf("loop depth: webhook=%d poll=%d (must match)", got, loopDepth(recP))
	}
	if loopDepth(recW) == 0 {
		t.Error("expected a loop to be detected on the repeated webhook delivery")
	}
}

// TestWebhookUnknownBotSenderUsesRouteHash verifies that when the sender is
// NOT a known bot, the webhook FromBot defaults to the route's bot hash (the
// receiver), exactly as the polled path behaves.
func TestWebhookUnknownBotSenderUsesRouteHash(t *testing.T) {
	t.Parallel()

	const routeHash = "hashRouteBot"
	rec := &recordingListener{}
	store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, capture.NewBotRegistry(0), rec)

	store.Record(context.Background(), webhookExchange(routeHash, 4242, 9999, false, "from a human", 7))

	if len(rec.events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.FromBot != routeHash {
		t.Errorf("FromBot = %q, want %q (route hash when sender is not a known bot)", ev.FromBot, routeHash)
	}
	if ev.ToBot != "" {
		t.Errorf("ToBot = %q, want empty (no known recipient)", ev.ToBot)
	}
}
