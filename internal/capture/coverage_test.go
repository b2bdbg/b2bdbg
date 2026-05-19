package capture_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/url"
	"testing"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

// -----------------------------------------------------------------------
// Encoding helpers
// -----------------------------------------------------------------------

// jsonResultMessage builds an "ok" response envelope wrapping a Message.
func jsonResultMessage(t *testing.T, result map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return b
}

// multipartBody encodes the given fields as multipart/form-data plus one
// binary "photo" part to prove the binary part is skipped gracefully.
func multipartBody(t *testing.T, fields map[string]string, withBinary bool) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("WriteField %s: %v", k, err)
		}
	}
	if withBinary {
		fw, err := mw.CreateFormFile("photo", "pic.jpg")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		_, _ = fw.Write([]byte{0xFF, 0xD8, 0xFF, 0x00, 0x01, 0x02, 0x03})
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// -----------------------------------------------------------------------
// Method × encoding coverage
// -----------------------------------------------------------------------

// TestMethodEncodingCoverage drives each first-class outbound method through
// every wire encoding Telegram clients use (JSON, x-www-form-urlencoded, and
// at least one multipart/form-data send* case) and asserts chat_id, the
// text/caption, the media key, the bot.to recipient and the resolution enum
// are all extracted correctly — not just for sendMessage.
func TestMethodEncodingCoverage(t *testing.T) {
	t.Parallel()

	const (
		botA       = "hashSender"
		botB       = "hashRecipient"
		botBUserID = int64(7001)
		chatID     = int64(7001) // == botBUserID so bot.to resolves
	)

	type want struct {
		chatID        int64
		text          string
		mediaKey      string
		toBot         string
		resolution    capture.Resolution
		fromChatID    int64
		syntheticChat bool // expect a derived negative ChatID (string chat_id)
	}

	cases := []struct {
		name     string
		method   string
		reqBody  []byte
		reqCT    string
		respBody []byte
		// registerBotB: register botB so bot.to resolves to it.
		registerBotB bool
		want         want
	}{
		{
			name:   "sendPhoto JSON with caption + photo media key",
			method: "sendPhoto",
			reqBody: mustJSON(t, map[string]any{
				"chat_id": chatID,
				"caption": "look at this",
			}),
			reqCT: "application/json",
			respBody: jsonResultMessage(t, map[string]any{
				"message_id": 1,
				"chat":       map[string]any{"id": chatID, "type": "private"},
				"caption":    "look at this",
				"photo": []any{
					map[string]any{"file_id": "small", "file_unique_id": "uq_small"},
					map[string]any{"file_id": "big", "file_unique_id": "uq_big"},
				},
			}),
			registerBotB: true,
			want:         want{chatID: chatID, text: "look at this", mediaKey: "uq_big", toBot: botB, resolution: capture.ResolutionResolved},
		},
		{
			name:   "sendDocument urlencoded with caption + document media key",
			method: "sendDocument",
			reqBody: []byte(url.Values{
				"chat_id": {"4242"},
				"caption": {"the report"},
			}.Encode()),
			reqCT: "application/x-www-form-urlencoded",
			respBody: jsonResultMessage(t, map[string]any{
				"message_id": 2,
				"chat":       map[string]any{"id": 4242, "type": "private"},
				"caption":    "the report",
				"document":   map[string]any{"file_id": "doc1", "file_unique_id": "uq_doc1"},
			}),
			want: want{chatID: 4242, text: "the report", mediaKey: "uq_doc1", resolution: capture.ResolutionUnknownGetMeNotSeen},
		},
		{
			name:   "sendVideo JSON no caption (non-text) keeps video media key",
			method: "sendVideo",
			reqBody: mustJSON(t, map[string]any{
				"chat_id": int64(-100200300), // group chat (negative)
			}),
			reqCT: "application/json",
			respBody: jsonResultMessage(t, map[string]any{
				"message_id": 3,
				"chat":       map[string]any{"id": -100200300, "type": "supergroup"},
				"video":      map[string]any{"file_id": "vid1", "file_unique_id": "uq_vid1"},
			}),
			want: want{chatID: -100200300, text: "", mediaKey: "uq_vid1", resolution: capture.ResolutionNonBotChat},
		},
		{
			name:   "sendPhoto multipart/form-data parses text/chat, skips binary",
			method: "sendPhoto",
			respBody: jsonResultMessage(t, map[string]any{
				"message_id": 4,
				"chat":       map[string]any{"id": chatID, "type": "private"},
				"caption":    "mp caption",
				"photo": []any{
					map[string]any{"file_id": "mp", "file_unique_id": "uq_mp"},
				},
			}),
			registerBotB: true,
			want:         want{chatID: chatID, text: "mp caption", mediaKey: "uq_mp", toBot: botB, resolution: capture.ResolutionResolved},
		},
		{
			name:   "copyMessage JSON captures from_chat_id + new caption",
			method: "copyMessage",
			reqBody: mustJSON(t, map[string]any{
				"chat_id":      chatID,
				"from_chat_id": int64(555),
				"message_id":   int64(88),
				"caption":      "copied caption",
			}),
			reqCT: "application/json",
			respBody: jsonResultMessage(t, map[string]any{
				"message_id": 5,
				"chat":       map[string]any{"id": chatID, "type": "private"},
			}),
			registerBotB: true,
			want:         want{chatID: chatID, text: "copied caption", fromChatID: 555, toBot: botB, resolution: capture.ResolutionResolved},
		},
		{
			name:   "forwardMessage urlencoded captures from_chat_id",
			method: "forwardMessage",
			reqBody: []byte(url.Values{
				"chat_id":      {"4242"},
				"from_chat_id": {"999"},
				"message_id":   {"12"},
			}.Encode()),
			reqCT: "application/x-www-form-urlencoded",
			respBody: jsonResultMessage(t, map[string]any{
				"message_id": 6,
				"chat":       map[string]any{"id": 4242, "type": "private"},
				"text":       "forwarded text",
			}),
			want: want{chatID: 4242, text: "forwarded text", fromChatID: 999, resolution: capture.ResolutionUnknownGetMeNotSeen},
		},
		{
			name:   "sendMessage to @channel string chat_id (synthetic id, still traced)",
			method: "sendMessage",
			reqBody: mustJSON(t, map[string]any{
				"chat_id": "@mychannel",
				"text":    "hi channel",
			}),
			reqCT:    "application/json",
			respBody: []byte(`{"ok":true}`),
			// chat_id is a string so a real numeric id is unavailable; the
			// event is still traced via a stable synthetic (negative) ChatID
			// and the resolution diagnostic is string_chat_id.
			want: want{chatID: 0, text: "hi channel", resolution: capture.ResolutionStringChatID, syntheticChat: true},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reqBody := tc.reqBody
			reqCT := tc.reqCT
			if reqBody == nil && tc.reqCT == "" {
				// multipart case: build the body here so the boundary matches.
				reqBody, reqCT = multipartBody(t, map[string]string{
					"chat_id": "7001",
					"caption": "mp caption",
				}, true)
			}

			reg := capture.NewBotRegistry(0)
			rec := &recordingListener{}
			store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
			ctx := context.Background()

			if tc.registerBotB {
				store.Record(ctx, getMeExchange(botB, botBUserID))
			}

			ex := &proxy.Exchange{
				Timestamp:      time.Now().UTC(),
				TokenHash:      botA,
				Method:         tc.method,
				ReqBody:        reqBody,
				ReqContentType: reqCT,
				RespBody:       tc.respBody,
				StatusCode:     200,
				Duration:       5 * time.Millisecond,
			}
			store.Record(ctx, ex)

			var ev *capture.Event
			for i := range rec.events {
				if rec.events[i].Method == tc.method {
					ev = &rec.events[i]
					break
				}
			}
			if ev == nil {
				t.Fatalf("no %s event recorded (events=%d)", tc.method, len(rec.events))
			}

			if tc.want.syntheticChat {
				if ev.ChatID >= 0 {
					t.Errorf("ChatID = %d, want a negative synthetic id for string chat_id", ev.ChatID)
				}
			} else if ev.ChatID != tc.want.chatID {
				t.Errorf("ChatID = %d, want %d", ev.ChatID, tc.want.chatID)
			}
			if ev.Text != tc.want.text {
				t.Errorf("Text = %q, want %q", ev.Text, tc.want.text)
			}
			if ev.MediaKey != tc.want.mediaKey {
				t.Errorf("MediaKey = %q, want %q", ev.MediaKey, tc.want.mediaKey)
			}
			if ev.ToBot != tc.want.toBot {
				t.Errorf("ToBot = %q, want %q", ev.ToBot, tc.want.toBot)
			}
			if ev.Resolution != tc.want.resolution {
				t.Errorf("Resolution = %q, want %q", ev.Resolution, tc.want.resolution)
			}
			if ev.FromChatID != tc.want.fromChatID {
				t.Errorf("FromChatID = %d, want %d", ev.FromChatID, tc.want.fromChatID)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// -----------------------------------------------------------------------
// Resolution enum — one explicit test per value
// -----------------------------------------------------------------------

// TestResolutionEnumValues asserts each of the four closed enum values is
// produced deterministically by the documented input, and that
// telegram.bot.to is non-empty IFF the value is "resolved".
func TestResolutionEnumValues(t *testing.T) {
	t.Parallel()

	const (
		sender     = "hashSenderBot"
		recipient  = "hashRecipientBot"
		recipientI = int64(9100)
	)

	send := func(t *testing.T, store *capture.Store, rec *recordingListener, chatID any) capture.Event {
		t.Helper()
		ex := &proxy.Exchange{
			Timestamp:      time.Now().UTC(),
			TokenHash:      sender,
			Method:         "sendMessage",
			ReqBody:        mustJSON(t, map[string]any{"chat_id": chatID, "text": "hi"}),
			ReqContentType: "application/json",
			RespBody:       []byte(`{"ok":true,"result":{"message_id":1}}`),
			StatusCode:     200,
		}
		store.Record(context.Background(), ex)
		if len(rec.events) == 0 {
			t.Fatal("no event recorded")
		}
		return rec.events[len(rec.events)-1]
	}

	t.Run("resolved — recipient is a known bot", func(t *testing.T) {
		t.Parallel()
		reg := capture.NewBotRegistry(0)
		rec := &recordingListener{}
		store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
		store.Record(context.Background(), getMeExchange(recipient, recipientI))
		ev := send(t, store, rec, recipientI)
		if ev.Resolution != capture.ResolutionResolved {
			t.Fatalf("Resolution = %q, want resolved", ev.Resolution)
		}
		if ev.ToBot != recipient {
			t.Errorf("ToBot = %q, want %q (must be non-empty when resolved)", ev.ToBot, recipient)
		}
	})

	t.Run("unknown_getme_not_seen — numeric id, registry miss", func(t *testing.T) {
		t.Parallel()
		reg := capture.NewBotRegistry(0)
		rec := &recordingListener{}
		store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
		ev := send(t, store, rec, int64(123456)) // positive, never seen via getMe
		if ev.Resolution != capture.ResolutionUnknownGetMeNotSeen {
			t.Fatalf("Resolution = %q, want unknown_getme_not_seen", ev.Resolution)
		}
		if ev.ToBot != "" {
			t.Errorf("ToBot = %q, want empty (never faked)", ev.ToBot)
		}
	})

	t.Run("non_bot_chat — negative group/channel id", func(t *testing.T) {
		t.Parallel()
		reg := capture.NewBotRegistry(0)
		rec := &recordingListener{}
		store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
		ev := send(t, store, rec, int64(-1001234567)) // supergroup id
		if ev.Resolution != capture.ResolutionNonBotChat {
			t.Fatalf("Resolution = %q, want non_bot_chat", ev.Resolution)
		}
		if ev.ToBot != "" {
			t.Errorf("ToBot = %q, want empty", ev.ToBot)
		}
	})

	t.Run("string_chat_id — @username", func(t *testing.T) {
		t.Parallel()
		reg := capture.NewBotRegistry(0)
		rec := &recordingListener{}
		store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
		ev := send(t, store, rec, "@somechannel")
		if ev.Resolution != capture.ResolutionStringChatID {
			t.Fatalf("Resolution = %q, want string_chat_id", ev.Resolution)
		}
		if ev.ToBot != "" {
			t.Errorf("ToBot = %q, want empty", ev.ToBot)
		}
	})

	t.Run("inbound resolved — known bot sender via getUpdates", func(t *testing.T) {
		t.Parallel()
		reg := capture.NewBotRegistry(0)
		rec := &recordingListener{}
		store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
		const senderBotID = int64(3300)
		store.Record(context.Background(), getMeExchange("hashA", senderBotID))
		store.Record(context.Background(), getUpdatesExchangeWithSender("hashB", 4400, senderBotID, "task", 1))
		last := rec.events[len(rec.events)-1]
		if last.Resolution != capture.ResolutionResolved {
			t.Fatalf("inbound Resolution = %q, want resolved", last.Resolution)
		}
		if last.ToBot == "" {
			t.Error("inbound ToBot empty though resolved")
		}
	})

	t.Run("inbound non_bot_chat — human sender", func(t *testing.T) {
		t.Parallel()
		reg := capture.NewBotRegistry(0)
		rec := &recordingListener{}
		store := capture.NewStoreWithRegistry(capture.StoreConfig{}, nil, reg, rec)
		store.Record(context.Background(), getUpdatesExchange("hashB", 4400, "from human", 1))
		last := rec.events[len(rec.events)-1]
		if last.Resolution != capture.ResolutionNonBotChat {
			t.Fatalf("inbound Resolution = %q, want non_bot_chat", last.Resolution)
		}
	})
}

// -----------------------------------------------------------------------
// Loop detection beyond text
// -----------------------------------------------------------------------

// TestCaptionLoopDetection verifies that a repeated caption (no text field)
// triggers loop detection — proving the loop signal derives from caption too.
func TestCaptionLoopDetection(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{
		LoopWindowSize: 20,
		LoopEntryTTL:   10 * time.Minute,
	}, nil, rec)
	ctx := context.Background()

	photoEx := func(caption string, msgID int64) *proxy.Exchange {
		return &proxy.Exchange{
			Timestamp:      time.Now().UTC(),
			TokenHash:      "botCap",
			Method:         "sendPhoto",
			ReqBody:        mustJSON(t, map[string]any{"chat_id": 1, "caption": caption}),
			ReqContentType: "application/json",
			RespBody: jsonResultMessage(t, map[string]any{
				"message_id": msgID,
				"chat":       map[string]any{"id": 1, "type": "private"},
				"caption":    caption,
				// Distinct file each time so only the caption can drive the loop.
				"photo": []any{map[string]any{"file_id": "f", "file_unique_id": "uq"}},
			}),
			StatusCode: 200,
		}
	}

	store.Record(ctx, photoEx("same caption", 1))
	store.Record(ctx, photoEx("same caption", 2)) // repeated caption → loop

	if len(rec.events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.events))
	}
	if rec.events[0].LoopDepth != 0 {
		t.Errorf("event[0] LoopDepth = %d, want 0", rec.events[0].LoopDepth)
	}
	if rec.events[1].LoopDepth == 0 {
		t.Error("event[1] LoopDepth = 0, want > 0 (repeated caption must loop)")
	}
}

// TestMediaFileIDLoopDetection verifies a repeated media file (same
// file_unique_id) with NO text/caption triggers loop detection.
func TestMediaFileIDLoopDetection(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{
		LoopWindowSize: 20,
		LoopEntryTTL:   10 * time.Minute,
	}, nil, rec)
	ctx := context.Background()

	docEx := func(uniqueID string, msgID int64) *proxy.Exchange {
		return &proxy.Exchange{
			Timestamp:      time.Now().UTC(),
			TokenHash:      "botMedia",
			Method:         "sendDocument",
			ReqBody:        mustJSON(t, map[string]any{"chat_id": 1}), // no text/caption
			ReqContentType: "application/json",
			RespBody: jsonResultMessage(t, map[string]any{
				"message_id": msgID,
				"chat":       map[string]any{"id": 1, "type": "private"},
				"document":   map[string]any{"file_id": "fid", "file_unique_id": uniqueID},
			}),
			StatusCode: 200,
		}
	}

	store.Record(ctx, docEx("UQ_SAME", 1))
	store.Record(ctx, docEx("UQ_OTHER", 2)) // different file → no loop
	store.Record(ctx, docEx("UQ_SAME", 3))  // same file_unique_id again → loop

	if len(rec.events) != 3 {
		t.Fatalf("got %d events, want 3", len(rec.events))
	}
	if rec.events[0].LoopDepth != 0 {
		t.Errorf("event[0] LoopDepth = %d, want 0", rec.events[0].LoopDepth)
	}
	if rec.events[1].LoopDepth != 0 {
		t.Errorf("event[1] LoopDepth = %d, want 0 (different file)", rec.events[1].LoopDepth)
	}
	if rec.events[2].LoopDepth == 0 {
		t.Error("event[2] LoopDepth = 0, want > 0 (repeated media file_unique_id must loop)")
	}
}

// TestNonMediaNoTextDoesNotLoop verifies the original behaviour is preserved:
// a message with neither text nor media never produces a loop signal even when
// repeated (e.g. a parameterless sendDice with no recognised media).
func TestNonMediaNoTextDoesNotLoop(t *testing.T) {
	t.Parallel()

	rec := &recordingListener{}
	store := capture.NewStore(capture.StoreConfig{
		LoopWindowSize: 20,
		LoopEntryTTL:   10 * time.Minute,
	}, nil, rec)
	ctx := context.Background()

	bare := func(msgID int64) *proxy.Exchange {
		return &proxy.Exchange{
			Timestamp:      time.Now().UTC(),
			TokenHash:      "botBare",
			Method:         "sendDice",
			ReqBody:        mustJSON(t, map[string]any{"chat_id": 1}),
			ReqContentType: "application/json",
			RespBody: jsonResultMessage(t, map[string]any{
				"message_id": msgID,
				"chat":       map[string]any{"id": 1, "type": "private"},
			}),
			StatusCode: 200,
		}
	}

	store.Record(ctx, bare(1))
	store.Record(ctx, bare(2))

	for i, ev := range rec.events {
		if ev.LoopDepth != 0 {
			t.Errorf("event[%d] LoopDepth = %d, want 0 (no text/media → no loop signal)", i, ev.LoopDepth)
		}
	}
}
