package capture

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/pkg/botapi"
)

// -----------------------------------------------------------------------
// bot.to resolution diagnostic
// -----------------------------------------------------------------------

// Resolution is the closed enum recorded on every captured message-bearing
// span as the b2b.bot.to.resolution attribute. It explains why
// telegram.bot.to is or is not populated, turning a silently-empty
// telegram.bot.to into an actionable diagnostic.
//
// The value is decided in the capture layer (where the recipient identity and
// the chat_id type are both known) and carried on [ParsedMessage.Resolution].
// When the value is [ResolutionResolved], telegram.bot.to is non-empty; for
// every other value telegram.bot.to is left empty and is never faked.
type Resolution string

const (
	// ResolutionResolved means telegram.bot.to was set: the recipient is a
	// known bot, learned from a prior getMe.
	ResolutionResolved Resolution = "resolved"

	// ResolutionUnknownGetMeNotSeen means the chat_id is numeric and could be a
	// bot, but no getMe for that id has been seen yet (registry miss). This is
	// the actionable case: route the recipient bot's getMe through b2bdbg.
	ResolutionUnknownGetMeNotSeen Resolution = "unknown_getme_not_seen"

	// ResolutionNonBotChat means the recipient is a human / group / channel
	// chat — resolution is not applicable.
	ResolutionNonBotChat Resolution = "non_bot_chat"

	// ResolutionStringChatID means the chat_id was an @username / channel
	// string, not a numeric id, so a bot hash cannot be derived.
	ResolutionStringChatID Resolution = "string_chat_id"
)

// classifyOutboundResolution decides the [Resolution] for an outbound
// message-bearing call (sendMessage and friends, forwardMessage, copyMessage).
//
// rawChatID is the chat_id exactly as it appeared in the request (string or
// numeric); numericChatID is its int64 coercion (0 when not numeric); toBot is
// the recipient hash already resolved by the caller; reg is the bot registry.
//
// The classification is deterministic and depends only on these inputs:
//   - toBot non-empty                       → resolved
//   - chat_id is a non-numeric string       → string_chat_id
//   - chat_id numeric & positive, reg miss  → unknown_getme_not_seen
//     (a positive id can be a bot/user; a negative id is a group/channel)
//   - otherwise                             → non_bot_chat
func classifyOutboundResolution(rawChatID any, numericChatID int64, toBot string, reg *BotRegistry) Resolution {
	if toBot != "" {
		return ResolutionResolved
	}
	if isStringChatID(rawChatID) {
		return ResolutionStringChatID
	}
	// Telegram bot/user ids are positive; group/supergroup/channel ids are
	// negative. Only a positive numeric id can plausibly be a bot whose getMe
	// we simply have not observed yet.
	if numericChatID > 0 {
		if reg != nil {
			if _, ok := reg.HashForID(numericChatID); ok {
				// Registered but toBot empty should not happen for outbound;
				// treat as resolved-but-self defensively as non_bot_chat.
				return ResolutionNonBotChat
			}
		}
		return ResolutionUnknownGetMeNotSeen
	}
	return ResolutionNonBotChat
}

// classifyInboundResolution decides the [Resolution] for an inbound update
// (getUpdates batch element or webhook delivery). For inbound traffic the
// recipient is always the receiving bot itself; telegram.bot.to is only set
// when the sender is a known bot (the A→B edge). When toBot is set the edge is
// fully resolved; otherwise the message came from a human/unknown sender so
// resolution is not applicable.
func classifyInboundResolution(toBot string) Resolution {
	if toBot != "" {
		return ResolutionResolved
	}
	return ResolutionNonBotChat
}

// isStringChatID reports whether the raw chat_id is a non-numeric string such
// as "@channelusername". A numeric string (e.g. "12345") is NOT treated as a
// string chat_id because Telegram accepts numeric ids in string form and a bot
// hash can still be derived from it.
func isStringChatID(raw any) bool {
	s, ok := raw.(string)
	if !ok {
		return false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// A leading '-' is allowed for negative numeric ids.
	for i, r := range s {
		if r == '-' && i == 0 {
			continue
		}
		if r < '0' || r > '9' {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------
// Parsed message — the structured result of parsing one Exchange
// -----------------------------------------------------------------------

// ParsedMessage is the result of extracting structured Bot API fields from a
// single [proxy.Exchange]. Fields that cannot be determined are left at their
// zero values.
type ParsedMessage struct {
	// Method is the Bot API method (e.g. "sendMessage", "getUpdates").
	Method string

	// ChatID is the Telegram chat identifier extracted from the request or
	// response body.
	ChatID int64

	// FromBot is the TokenHash of the bot that made the request.
	FromBot string

	// ToBot is the TokenHash of the bot that will receive (or has received) the
	// message. For outbound sendMessage calls it is inferred from the response
	// From.ID if the recipient is also a known bot. For getUpdates responses it
	// is the caller's token hash.
	ToBot string

	// MessageID is the Telegram message identifier.
	MessageID int64

	// Text is the message text (sendMessage) or the first text seen in a
	// getUpdates response batch. For non-text messages it is the caption when
	// one is present.
	Text string

	// TextLen is len(Text) in bytes.
	TextLen int

	// MediaKey is a stable media identity (file_unique_id, falling back to
	// file_id) for non-text messages. It feeds loop detection so repeated
	// media/file hand-offs are detected, not only repeated text. Empty when
	// the message carries no recognised media.
	MediaKey string

	// MediaKind is the coarse media classification when the message carries a
	// recognised attachment: one of "photo", "document", "video". Empty for
	// text-only messages and for media kinds parse.go does not model. It is
	// derived alongside MediaKey from the same message.
	MediaKind string

	// UpdateID is the Telegram Update.update_id for inbound updates (a
	// getUpdates batch element or a webhook delivery). It is 0 for outbound
	// sends (which carry no update_id) and is only emitted when > 0.
	UpdateID int64

	// Resolution explains why ToBot is or is not populated. It is emitted as
	// the b2b.bot.to.resolution span attribute. See [Resolution].
	Resolution Resolution

	// FromChatID is the origin chat for forwardMessage / copyMessage calls
	// (0 when not applicable or not present).
	FromChatID int64

	// ThreadID is the optional message thread identifier.
	ThreadID int64

	// Timestamp is copied from Exchange.Timestamp.
	Timestamp time.Time

	// Duration is copied from Exchange.Duration.
	Duration time.Duration

	// StatusCode is the HTTP response status code.
	StatusCode int

	// Truncated is copied from [proxy.Exchange.Truncated]: true when the tapped
	// request/response body copy hit the proxy body cap, so the fields parsed
	// here may be incomplete. Emitted as the b2b.capture.truncated span
	// attribute.
	Truncated bool
}

// parseExchange converts a raw [proxy.Exchange] into a [ParsedMessage].
// Parsing is best-effort: if a field cannot be decoded it is left at its zero
// value and no error is returned.
//
// reg is optional; when non-nil it is consulted to resolve and record bot
// identity mappings so that telegram.bot.to can be set on outbound messages.
func parseExchange(ex *proxy.Exchange, reg *BotRegistry) ParsedMessage {
	pm := ParsedMessage{
		Method:     ex.Method,
		FromBot:    ex.TokenHash,
		Timestamp:  ex.Timestamp,
		Duration:   ex.Duration,
		StatusCode: ex.StatusCode,
		Truncated:  ex.Truncated,
	}

	switch ex.Method {
	case "sendMessage", "sendPhoto", "sendAudio", "sendDocument", "sendVideo",
		"sendAnimation", "sendVoice", "sendVideoNote", "sendPoll", "sendDice",
		"sendSticker":
		parseSendMessage(ex, &pm, reg)
	case "forwardMessage", "copyMessage":
		parseForwardOrCopy(ex, &pm, reg)
	case "getUpdates":
		parseGetUpdates(ex, &pm, reg)
	case "getMe":
		parseGetMe(ex, reg)
	case proxy.MethodWebhookIngress:
		pms := parseWebhookUpdate(ex, reg)
		if len(pms) > 0 {
			first := pms[0]
			pm.ChatID = first.ChatID
			pm.MessageID = first.MessageID
			pm.Text = first.Text
			pm.MediaKey = first.MediaKey
			pm.MediaKind = first.MediaKind
			pm.UpdateID = first.UpdateID
			pm.ThreadID = first.ThreadID
			pm.FromBot = first.FromBot
			pm.ToBot = first.ToBot
			pm.Resolution = first.Resolution
		}
	default:
		// For other methods, try to pull a chat_id from the request body.
		parseGeneric(ex, &pm)
	}

	pm.TextLen = len(pm.Text)
	return pm
}

// outboundFields holds the chat/text/media identity extracted from an
// outbound request body, independent of the wire encoding.
type outboundFields struct {
	rawChatID     any    // chat_id exactly as it appeared (string or numeric)
	rawFromChatID any    // from_chat_id (forwardMessage / copyMessage)
	text          string // text or caption
	threadID      int64
}

// decodeOutboundBody extracts the outbound fields from a request body in any
// of the three encodings Telegram clients use: application/json,
// application/x-www-form-urlencoded, and multipart/form-data. The encoding is
// chosen from the Content-Type; an unknown/blank Content-Type falls back to a
// JSON attempt then a form attempt so capture stays best-effort.
//
// For multipart bodies only the text/chat fields are parsed; binary file
// parts are skipped (Telegram puts them in their own parts) and the existing
// proxy body cap still bounds how much is ever read.
func decodeOutboundBody(ex *proxy.Exchange) (outboundFields, bool) {
	if len(ex.ReqBody) == 0 {
		return outboundFields{}, false
	}

	mediaType, params, _ := mime.ParseMediaType(ex.ReqContentType)

	switch {
	case mediaType == "application/json":
		return decodeOutboundJSON(ex.ReqBody)
	case mediaType == "application/x-www-form-urlencoded":
		return decodeOutboundForm(ex.ReqBody)
	case strings.HasPrefix(mediaType, "multipart/"):
		if f, ok := decodeOutboundMultipart(ex.ReqBody, params["boundary"]); ok {
			return f, true
		}
		return outboundFields{}, false
	default:
		// Unknown/blank Content-Type: try JSON first, then urlencoded.
		if f, ok := decodeOutboundJSON(ex.ReqBody); ok {
			return f, true
		}
		return decodeOutboundForm(ex.ReqBody)
	}
}

func decodeOutboundJSON(body []byte) (outboundFields, bool) {
	var raw struct {
		ChatID     any    `json:"chat_id"`
		FromChatID any    `json:"from_chat_id"`
		Text       string `json:"text"`
		Caption    string `json:"caption"`
		ThreadID   int64  `json:"message_thread_id"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return outboundFields{}, false
	}
	txt := raw.Text
	if txt == "" {
		txt = raw.Caption
	}
	return outboundFields{
		rawChatID:     raw.ChatID,
		rawFromChatID: raw.FromChatID,
		text:          txt,
		threadID:      raw.ThreadID,
	}, true
}

func decodeOutboundForm(body []byte) (outboundFields, bool) {
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return outboundFields{}, false
	}
	return outboundFieldsFromValues(vals.Get), true
}

func decodeOutboundMultipart(body []byte, boundary string) (outboundFields, bool) {
	if boundary == "" {
		return outboundFields{}, false
	}
	mr := multipart.NewReader(strings.NewReader(string(body)), boundary)
	get := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break // io.EOF or a truncated final part (body cap) — stop gracefully
		}
		name := part.FormName()
		if name == "" {
			_ = part.Close()
			continue
		}
		// Only read recognised text fields; skip binary file parts entirely.
		switch name {
		case "chat_id", "from_chat_id", "text", "caption", "message_thread_id":
			buf := make([]byte, 4096)
			n, _ := io.ReadFull(part, buf)
			get[name] = string(buf[:n])
		}
		_ = part.Close()
	}
	return outboundFieldsFromValues(func(k string) string { return get[k] }), true
}

func outboundFieldsFromValues(get func(string) string) outboundFields {
	txt := get("text")
	if txt == "" {
		txt = get("caption")
	}
	var threadID int64
	if v := get("message_thread_id"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &threadID)
	}
	var f outboundFields
	if v := get("chat_id"); v != "" {
		f.rawChatID = v
	}
	if v := get("from_chat_id"); v != "" {
		f.rawFromChatID = v
	}
	f.text = txt
	f.threadID = threadID
	return f
}

// parseSendMessage extracts fields from a sendMessage (or similar single-media
// outbound) exchange across all three wire encodings. It populates Text (or
// the caption for non-text messages), a stable MediaKey for media messages,
// and the bot.to recipient resolution diagnostic.
func parseSendMessage(ex *proxy.Exchange, pm *ParsedMessage, reg *BotRegistry) {
	var rawChatID any
	if f, ok := decodeOutboundBody(ex); ok {
		rawChatID = f.rawChatID
		pm.ChatID = chatIDInt(f.rawChatID)
		pm.Text = f.text
		pm.ThreadID = f.threadID
	}

	// Parse response body for message_id, chat, text/caption and media id.
	if len(ex.RespBody) > 0 {
		var resp struct {
			OK     bool            `json:"ok"`
			Result *botapi.Message `json:"result"`
		}
		if err := json.Unmarshal(ex.RespBody, &resp); err == nil && resp.OK && resp.Result != nil {
			r := resp.Result
			pm.MessageID = r.MessageID
			if pm.ChatID == 0 {
				pm.ChatID = r.Chat.ID
			}
			if pm.Text == "" {
				pm.Text = r.Text
			}
			if pm.Text == "" {
				pm.Text = r.Caption
			}
			pm.MediaKey = mediaKeyOf(r)
			pm.MediaKind = mediaKindOf(r)
		}
	}

	resolveOutboundToBot(pm, rawChatID, reg, classifyOutboundResolution)
}

// parseForwardOrCopy handles forwardMessage and copyMessage. In addition to
// the destination chat_id it captures the origin chat (from_chat_id) so the
// hand-off edge is visible, and copies the caption when copyMessage rewrites
// one.
func parseForwardOrCopy(ex *proxy.Exchange, pm *ParsedMessage, reg *BotRegistry) {
	var rawChatID any
	if f, ok := decodeOutboundBody(ex); ok {
		rawChatID = f.rawChatID
		pm.ChatID = chatIDInt(f.rawChatID)
		pm.FromChatID = chatIDInt(f.rawFromChatID)
		pm.Text = f.text
		pm.ThreadID = f.threadID
	}

	if len(ex.RespBody) > 0 {
		var resp struct {
			OK     bool            `json:"ok"`
			Result *botapi.Message `json:"result"`
		}
		if err := json.Unmarshal(ex.RespBody, &resp); err == nil && resp.OK && resp.Result != nil {
			r := resp.Result
			pm.MessageID = r.MessageID
			if pm.ChatID == 0 {
				pm.ChatID = r.Chat.ID
			}
			if pm.Text == "" {
				pm.Text = r.Text
			}
			if pm.Text == "" {
				pm.Text = r.Caption
			}
			if pm.FromChatID == 0 && r.ForwardFromChat != nil {
				pm.FromChatID = r.ForwardFromChat.ID
			}
			pm.MediaKey = mediaKeyOf(r)
			pm.MediaKind = mediaKindOf(r)
		}
	}

	resolveOutboundToBot(pm, rawChatID, reg, classifyOutboundResolution)
}

// resolveOutboundToBot resolves pm.ToBot from the registry (when the numeric
// destination chat_id is a known bot) and then records the resolution
// diagnostic via classify.
func resolveOutboundToBot(
	pm *ParsedMessage,
	rawChatID any,
	reg *BotRegistry,
	classify func(raw any, numeric int64, toBot string, reg *BotRegistry) Resolution,
) {
	if reg != nil && pm.ChatID != 0 {
		if hash, ok := reg.HashForID(pm.ChatID); ok {
			pm.ToBot = hash
		}
	}
	pm.Resolution = classify(rawChatID, pm.ChatID, pm.ToBot, reg)

	// A send to an @username / channel string carries no numeric chat_id, so
	// the conversation correlator (which keys on int64 ChatID) would otherwise
	// drop the event and the string_chat_id diagnostic would never surface.
	// Derive a stable, collision-resistant synthetic ChatID from the string so
	// the hop is still traced. It is forced negative so it can never collide
	// with a real positive bot/user id, and a real chat id is always preferred.
	if pm.ChatID == 0 {
		if s, ok := rawChatID.(string); ok && strings.TrimSpace(s) != "" {
			pm.ChatID = syntheticChatID(strings.TrimSpace(s))
		}
	}
}

// syntheticChatID derives a deterministic, always-negative int64 from a string
// chat_id (e.g. "@channel"). Negative so it never collides with a real
// positive bot/user id; deterministic so repeated sends to the same channel
// correlate into one conversation.
func syntheticChatID(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	v := int64(h.Sum64() & 0x7FFFFFFFFFFFFFFF)
	if v == 0 {
		v = 1
	}
	return -v
}

// mediaKeyOf returns a stable media identity for a message: the largest
// photo's / document's / video's file_unique_id, falling back to file_id.
// Empty when the message carries no recognised media.
func mediaKeyOf(m *botapi.Message) string {
	if m == nil {
		return ""
	}
	pick := func(uniq, id string) string {
		if uniq != "" {
			return uniq
		}
		return id
	}
	if n := len(m.Photo); n > 0 {
		// Last element is the largest size.
		p := m.Photo[n-1]
		if k := pick(p.FileUniqueID, p.FileID); k != "" {
			return k
		}
	}
	if m.Document != nil {
		if k := pick(m.Document.FileUniqueID, m.Document.FileID); k != "" {
			return k
		}
	}
	if m.Video != nil {
		if k := pick(m.Video.FileUniqueID, m.Video.FileID); k != "" {
			return k
		}
	}
	return ""
}

// mediaKindOf returns the coarse media classification for a message using the
// SAME recognition order and conditions as [mediaKeyOf] so the emitted
// telegram.media.kind can never disagree with the presence of a MediaKey. It
// returns "photo", "document" or "video", or "" when the message carries no
// recognised media (text-only or a kind not modelled here).
func mediaKindOf(m *botapi.Message) string {
	if m == nil {
		return ""
	}
	pick := func(uniq, id string) string {
		if uniq != "" {
			return uniq
		}
		return id
	}
	if n := len(m.Photo); n > 0 {
		p := m.Photo[n-1]
		if pick(p.FileUniqueID, p.FileID) != "" {
			return "photo"
		}
	}
	if m.Document != nil {
		if pick(m.Document.FileUniqueID, m.Document.FileID) != "" {
			return "document"
		}
	}
	if m.Video != nil {
		if pick(m.Video.FileUniqueID, m.Video.FileID) != "" {
			return "video"
		}
	}
	return ""
}

// parseGetMe extracts the bot's own user ID from a getMe response and registers
// it in the registry so subsequent sendMessage calls targeting this bot can
// resolve telegram.bot.to.
//
// getMe carries no chat_id so the resulting ParsedMessage will be skipped by
// the Store (ChatID==0). The only meaningful side-effect is populating reg.
func parseGetMe(ex *proxy.Exchange, reg *BotRegistry) {
	if reg == nil || len(ex.RespBody) == 0 || ex.TokenHash == "" {
		return
	}

	var resp struct {
		OK     bool         `json:"ok"`
		Result *botapi.User `json:"result"`
	}
	if err := json.Unmarshal(ex.RespBody, &resp); err != nil || !resp.OK || resp.Result == nil {
		return
	}
	if resp.Result.ID != 0 {
		reg.Register(resp.Result.ID, ex.TokenHash)
	}
}

// parseGetUpdates extracts fields from the first message-bearing update in a
// getUpdates exchange and writes them into pm.  It is used by parseExchange
// when only a single ParsedMessage is required (e.g. for non-batch callers).
// For the authoritative per-update path use parseGetUpdatesAll.
func parseGetUpdates(ex *proxy.Exchange, pm *ParsedMessage, reg *BotRegistry) {
	pms := parseGetUpdatesAll(ex, reg)
	if len(pms) == 0 {
		return
	}
	// Copy the first result's fields into the caller-provided pm.
	first := pms[0]
	pm.ChatID = first.ChatID
	pm.MessageID = first.MessageID
	pm.Text = first.Text
	pm.MediaKey = first.MediaKey
	pm.MediaKind = first.MediaKind
	pm.UpdateID = first.UpdateID
	pm.ThreadID = first.ThreadID
	pm.FromBot = first.FromBot
	pm.ToBot = first.ToBot
}

// parseGetUpdatesAll parses a getUpdates response and returns one
// [ParsedMessage] per update that contains a message.  Each ParsedMessage
// carries the correct per-update From/To bot resolution so that every hop is
// individually traceable — even when Telegram batches multiple updates into a
// single long-poll response.
//
// When reg is non-nil, the sender of each incoming message is checked against
// the registry: if the sender is a known bot the FromBot/ToBot fields are
// swapped so the A→B edge is visible for that specific message.
//
// An empty slice is returned when the response body is absent, the JSON is
// malformed, or no update contains a usable message.
func parseGetUpdatesAll(ex *proxy.Exchange, reg *BotRegistry) []ParsedMessage {
	if len(ex.RespBody) == 0 {
		return nil
	}

	var resp struct {
		OK     bool            `json:"ok"`
		Result []botapi.Update `json:"result"`
	}
	if err := json.Unmarshal(ex.RespBody, &resp); err != nil || !resp.OK {
		return nil
	}

	out := make([]ParsedMessage, 0, len(resp.Result))
	for _, u := range resp.Result {
		if pm, ok := parseInboundUpdate(ex, u, reg); ok {
			out = append(out, pm)
		}
	}
	return out
}

// parseWebhookUpdate parses a Telegram webhook delivery and returns the
// resulting [ParsedMessage]s.
//
// Telegram delivers a webhook as a single [botapi.Update] JSON object in the
// request body — exactly one element of the array a getUpdates response would
// carry. parseWebhookUpdate therefore reuses [parseInboundUpdate], the same
// per-update resolution used by [parseGetUpdatesAll], so a webhook update from
// a known bot A delivered to bot B's route produces the identical
// FromBot=hash(A) / ToBot=hash(B) edge and the identical loop-detection input
// that the polled equivalent would.
//
// ex.TokenHash is the hash of the route's configured bot token (the receiving
// bot, i.e. the polling-bot analogue). An empty slice is returned when the
// body is absent, malformed, or carries no usable message.
func parseWebhookUpdate(ex *proxy.Exchange, reg *BotRegistry) []ParsedMessage {
	if len(ex.ReqBody) == 0 {
		return nil
	}

	var u botapi.Update
	if err := json.Unmarshal(ex.ReqBody, &u); err != nil {
		return nil
	}

	pm, ok := parseInboundUpdate(ex, u, reg)
	if !ok {
		return nil
	}
	return []ParsedMessage{pm}
}

// parseInboundUpdate converts a single Telegram [botapi.Update] (as delivered
// either inside a getUpdates response batch or as a standalone webhook body)
// into a [ParsedMessage].
//
// ex.TokenHash is the receiving bot's token hash (the poller, or the webhook
// route's configured bot). When reg is non-nil and the message sender is a
// known bot, FromBot/ToBot are swapped so the A→B edge is recorded for this
// specific message. The boolean result is false when the update carries no
// usable message.
//
// Both the long-poll and webhook ingress paths funnel through this function so
// that capture, loop detection, conversation correlation and telemetry behave
// identically regardless of how the update reached b2bdbg.
func parseInboundUpdate(ex *proxy.Exchange, u botapi.Update, reg *BotRegistry) (ParsedMessage, bool) {
	msg := firstMessage(u)
	if msg == nil {
		return ParsedMessage{}, false
	}

	// Text falls back to the caption for non-text (media) messages so loop
	// detection and token estimation still have a signal.
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	pm := ParsedMessage{
		Method:     ex.Method,
		FromBot:    ex.TokenHash, // default: receiving bot is the "from"
		Timestamp:  ex.Timestamp,
		Duration:   ex.Duration,
		StatusCode: ex.StatusCode,
		ChatID:     msg.Chat.ID,
		MessageID:  msg.MessageID,
		Text:       text,
		MediaKey:   mediaKeyOf(msg),
		MediaKind:  mediaKindOf(msg),
		UpdateID:   u.UpdateID,
		Truncated:  ex.Truncated,
	}
	if msg.MessageThreadID != 0 {
		pm.ThreadID = msg.MessageThreadID
	}

	// Resolve the sender: if it is a known bot, set FromBot as the actual
	// message originator and ToBot as the receiving bot so the A→B edge
	// is recorded for this specific message.
	if reg != nil && msg.From != nil && msg.From.ID != 0 && msg.From.IsBot {
		if senderHash, ok := reg.HashForID(msg.From.ID); ok && senderHash != ex.TokenHash {
			pm.FromBot = senderHash
			pm.ToBot = ex.TokenHash
		}
	}

	pm.Resolution = classifyInboundResolution(pm.ToBot)
	pm.TextLen = len(pm.Text)
	return pm, true
}

// parseGeneric tries to extract a chat_id from an arbitrary request body.
func parseGeneric(ex *proxy.Exchange, pm *ParsedMessage) {
	if len(ex.ReqBody) == 0 {
		return
	}
	// Best-effort: try to unmarshal a struct with just chat_id.
	var partial struct {
		ChatID any    `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(ex.ReqBody, &partial); err == nil {
		pm.ChatID = chatIDInt(partial.ChatID)
		pm.Text = partial.Text
	}
}

// firstMessage returns the Message from an Update, trying Message then
// EditedMessage then ChannelPost.
func firstMessage(u botapi.Update) *botapi.Message {
	if u.Message != nil {
		return u.Message
	}
	if u.EditedMessage != nil {
		return u.EditedMessage
	}
	if u.ChannelPost != nil {
		return u.ChannelPost
	}
	return nil
}

// chatIDInt coerces a Bot API chat_id (which may be a JSON number or string)
// to int64.
func chatIDInt(v any) int64 {
	if v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		// Numeric string — attempt parse.
		var n int64
		_, _ = fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

// -----------------------------------------------------------------------
// Text hashing helper for loop detection
// -----------------------------------------------------------------------

// textHash returns a compact FNV-64a hash of the message text, represented as
// a hex string. An empty text returns an empty string.
func textHash(text string) string {
	if text == "" {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return fmt.Sprintf("%x", h.Sum64())
}

// -----------------------------------------------------------------------
// Logger-aware no-op defaults
// -----------------------------------------------------------------------

// discardLogger returns a logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
