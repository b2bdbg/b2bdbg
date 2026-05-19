// Package botapi contains minimal, public Telegram Bot API type definitions
// used as span attributes and for request/response parsing inside the proxy.
//
// Types are intentionally kept small — only the fields b2bdbg actually reads —
// so that consumers of this module do not need to import a full Bot API SDK.
//
// JSON field names match the Telegram Bot API 7.x schema exactly.
package botapi

import "strings"

// ParseMethod extracts the Bot API method name from an incoming request path.
//
// The Telegram Bot API path format is:
//
//	/bot<TOKEN>/<MethodName>
//
// ParseMethod returns the method name (e.g. "getMe", "sendMessage") and the
// raw bot token. If the path is not in the expected format an empty string is
// returned for both values.
func ParseMethod(path string) (method, token string) {
	// Strip leading slash.
	p := strings.TrimPrefix(path, "/")

	// Must start with "bot".
	if !strings.HasPrefix(p, "bot") {
		return "", ""
	}
	p = p[len("bot"):]

	// Find the slash separating token from method.
	idx := strings.IndexByte(p, '/')
	if idx < 0 {
		// No method segment — treat as no-op (e.g. just /bot<TOKEN>).
		return "", ""
	}

	token = p[:idx]
	method = p[idx+1:]

	// Strip any trailing path segments (e.g. /bot<TOKEN>/sendMessage/extra).
	if i := strings.IndexByte(method, '/'); i >= 0 {
		method = method[:i]
	}

	return method, token
}

// -----------------------------------------------------------------------
// Core Telegram types
// -----------------------------------------------------------------------

// User represents a Telegram user or bot account (Bot API User object).
type User struct {
	// ID is the unique identifier for this user/bot.
	ID int64 `json:"id"`

	// IsBot is true if this user is a bot.
	IsBot bool `json:"is_bot"`

	// FirstName is the user's or bot's first name.
	FirstName string `json:"first_name"`

	// LastName is the user's or bot's last name (optional).
	LastName string `json:"last_name,omitempty"`

	// Username is the user's or bot's username (optional).
	Username string `json:"username,omitempty"`

	// LanguageCode is the IETF language tag of the user's language (optional).
	LanguageCode string `json:"language_code,omitempty"`
}

// Chat represents a Telegram chat (Bot API Chat object, minimal subset).
type Chat struct {
	// ID is the unique identifier for the chat.
	ID int64 `json:"id"`

	// Type is the type of chat: "private", "group", "supergroup", "channel".
	Type string `json:"type"`

	// Title is the title for supergroups, channels and group chats (optional).
	Title string `json:"title,omitempty"`

	// Username is the username for private chats, supergroups, or channels (optional).
	Username string `json:"username,omitempty"`
}

// PhotoSize represents one size of a photo or file/sticker thumbnail
// (Bot API PhotoSize object, minimal subset).
type PhotoSize struct {
	// FileID is the identifier for this file, usable to download or reuse it.
	FileID string `json:"file_id"`

	// FileUniqueID is stable over time and across bots; it cannot be used to
	// download or reuse the file.
	FileUniqueID string `json:"file_unique_id"`
}

// Document represents a general file (Bot API Document object, minimal subset).
type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
}

// Video represents a video file (Bot API Video object, minimal subset).
type Video struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
}

// Message represents a Telegram message (Bot API Message object, minimal subset).
type Message struct {
	// MessageID is the unique message identifier inside the chat.
	MessageID int64 `json:"message_id"`

	// From is the sender of the message; empty for channel messages.
	From *User `json:"from,omitempty"`

	// Chat is the conversation the message belongs to.
	Chat Chat `json:"chat"`

	// Date is Unix time when the message was sent.
	Date int64 `json:"date"`

	// Text is the actual UTF-8 text of the message, 0–4096 characters (optional).
	Text string `json:"text,omitempty"`

	// Caption is the caption for an animation, audio, document, photo, video or
	// voice message (optional).
	Caption string `json:"caption,omitempty"`

	// Photo is the available sizes of the photo (optional). The last element is
	// the largest size.
	Photo []PhotoSize `json:"photo,omitempty"`

	// Document is the attached general file (optional).
	Document *Document `json:"document,omitempty"`

	// Video is the attached video (optional).
	Video *Video `json:"video,omitempty"`

	// ForwardFromChat is the chat the message was originally sent in, when the
	// message is a forward from a channel or another chat (optional, legacy
	// Bot API field, still emitted alongside forward_origin).
	ForwardFromChat *Chat `json:"forward_from_chat,omitempty"`

	// ReplyToMessage is the replied-to message (optional).
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`

	// MessageThreadID is the unique identifier of the message thread (optional).
	MessageThreadID int64 `json:"message_thread_id,omitempty"`
}

// Update represents an incoming update from Telegram (Bot API Update object).
type Update struct {
	// UpdateID is the update's unique identifier.
	UpdateID int64 `json:"update_id"`

	// Message is a new incoming message (optional).
	Message *Message `json:"message,omitempty"`

	// EditedMessage is a new version of a message (optional).
	EditedMessage *Message `json:"edited_message,omitempty"`

	// ChannelPost is a new incoming channel post (optional).
	ChannelPost *Message `json:"channel_post,omitempty"`
}

// -----------------------------------------------------------------------
// Method request types
// -----------------------------------------------------------------------

// GetUpdatesRequest represents the parameters for the getUpdates method.
type GetUpdatesRequest struct {
	// Offset is the identifier of the first update to be returned.
	Offset int64 `json:"offset,omitempty"`

	// Limit is the maximum number of updates to be retrieved (1–100, default 100).
	Limit int `json:"limit,omitempty"`

	// Timeout is the timeout in seconds for long polling (0 = short poll).
	Timeout int `json:"timeout,omitempty"`

	// AllowedUpdates is a list of update types to receive (optional).
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

// SendMessageRequest represents the parameters for the sendMessage method.
type SendMessageRequest struct {
	// ChatID is the target chat identifier (integer or @username string).
	ChatID any `json:"chat_id"`

	// Text is the text of the message (1–4096 characters).
	Text string `json:"text"`

	// ParseMode is the mode for parsing entities in the message text (optional).
	ParseMode string `json:"parse_mode,omitempty"`

	// ReplyToMessageID is the identifier of the original message (optional).
	ReplyToMessageID int64 `json:"reply_to_message_id,omitempty"`

	// MessageThreadID is the unique identifier for the target message thread (optional).
	MessageThreadID int64 `json:"message_thread_id,omitempty"`
}

// SendMediaRequest represents the shared parameters of the media-bearing send
// methods (sendPhoto, sendDocument, sendVideo, sendAudio, sendAnimation,
// sendVoice, ...). Only the fields b2bdbg reads are modelled; the binary file
// fields are intentionally omitted.
type SendMediaRequest struct {
	// ChatID is the target chat identifier (integer or @username string).
	ChatID any `json:"chat_id"`

	// Caption is the optional caption that accompanies the media (0–1024 chars).
	Caption string `json:"caption,omitempty"`

	// MessageThreadID is the unique identifier for the target message thread
	// (optional).
	MessageThreadID int64 `json:"message_thread_id,omitempty"`
}

// ForwardMessageRequest represents the parameters for the forwardMessage method.
type ForwardMessageRequest struct {
	// ChatID is the destination chat identifier.
	ChatID any `json:"chat_id"`

	// FromChatID is the chat the original message was sent in.
	FromChatID any `json:"from_chat_id"`

	// MessageID is the identifier of the message to forward.
	MessageID int64 `json:"message_id,omitempty"`

	// MessageThreadID is the unique identifier for the target message thread
	// (optional).
	MessageThreadID int64 `json:"message_thread_id,omitempty"`
}

// CopyMessageRequest represents the parameters for the copyMessage method.
type CopyMessageRequest struct {
	// ChatID is the destination chat identifier.
	ChatID any `json:"chat_id"`

	// FromChatID is the chat the original message was sent in.
	FromChatID any `json:"from_chat_id"`

	// MessageID is the identifier of the message to copy.
	MessageID int64 `json:"message_id,omitempty"`

	// Caption is the optional new caption for the copied media.
	Caption string `json:"caption,omitempty"`

	// MessageThreadID is the unique identifier for the target message thread
	// (optional).
	MessageThreadID int64 `json:"message_thread_id,omitempty"`
}

// -----------------------------------------------------------------------
// Response envelope
// -----------------------------------------------------------------------

// Response is the generic Bot API response envelope.
//
// All Bot API methods return responses in this form. On success, the result
// field contains a method-specific JSON object.
type Response struct {
	// OK is true if the request was successful.
	OK bool `json:"ok"`

	// Description contains a human-readable description of the error (when OK=false).
	Description string `json:"description,omitempty"`

	// ErrorCode is the error code (when OK=false).
	ErrorCode int `json:"error_code,omitempty"`

	// Result holds the method-specific result when OK=true.
	// Callers unmarshal this into the concrete type they expect.
	Result any `json:"result,omitempty"`
}
