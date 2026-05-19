// Package mocktelegram implements a minimal in-process Telegram Bot API stand-in.
//
// It supports just the methods the support-team example bots use:
//
//   - getMe         — returns a fixed User for the token.
//   - getUpdates    — returns queued updates; wakes long-poll callers on delivery.
//   - sendMessage   — delivers a message to the target chat's update queue.
//   - setWebhook    — accepted, no-op (returns ok:true).
//   - deleteWebhook — accepted, no-op (returns ok:true).
//
// Requests are accepted as application/x-www-form-urlencoded (the format used
// by go-telegram-bot-api/v5) or as application/json.
//
// Bot tokens are arbitrary strings; each unique token gets an independent
// identity and update queue. Bots can address each other by using a numeric
// chat_id equal to the target bot's user ID (assigned deterministically from
// the registration order).
//
// The mock is bounded: each bot's update queue holds at most MaxQueueDepth
// updates; excess pushes are silently dropped to prevent unbounded memory use.
package mocktelegram

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MaxQueueDepth is the maximum number of pending updates per bot.
const MaxQueueDepth = 256

// Server is an in-process Telegram Bot API mock.
// Construct via [New]; do not copy after first use.
// It implements [http.Handler] so it can be passed directly to httptest.NewServer
// or any *http.Server.
type Server struct {
	mu       sync.Mutex
	bots     map[string]*botState // key: raw token
	nextID   atomic.Int64         // monotone update/message ID generator
	nextUser atomic.Int64         // monotone user/bot ID
	log      *slog.Logger
}

// botState holds the per-token identity and pending update queue.
type botState struct {
	user      botUser
	queue     []update        // bounded FIFO
	listeners []chan struct{} // signalled when a new update arrives
}

// New constructs a [Server] with the given logger.
// logger may be nil (output is discarded).
func New(logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	s := &Server{
		bots: make(map[string]*botState),
		log:  logger,
	}
	s.nextID.Store(1)
	s.nextUser.Store(1000) // start user IDs at 1000 to avoid collisions
	return s
}

// RegisterBot pre-registers a token and returns its deterministic user ID.
// Calling RegisterBot before ServeHTTP ensures the bot's ID is known before
// the first getMe call. Safe to call from multiple goroutines.
func (s *Server) RegisterBot(token string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureBot(token).user.ID
}

// BotID returns the user ID assigned to the given token, registering if needed.
func (s *Server) BotID(token string) int64 { return s.RegisterBot(token) }

// ServeHTTP implements http.Handler. It routes /bot<TOKEN>/method paths.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, method := parsePath(r.URL.Path)
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	s.log.Debug(
		"mocktelegram: request",
		slog.String("method", method),
		slog.String("token_prefix", safePrefix(token)),
	)

	switch method {
	case "getMe":
		s.handleGetMe(w, r, token)
	case "getUpdates":
		s.handleGetUpdates(w, r, token)
	case "sendMessage":
		s.handleSendMessage(w, r, token)
	case "setWebhook", "deleteWebhook":
		writeOK(w, true)
	default:
		writeError(w, http.StatusOK, fmt.Sprintf("method %q not implemented in mock", method))
	}
}

// -----------------------------------------------------------------------
// Method handlers
// -----------------------------------------------------------------------

func (s *Server) handleGetMe(w http.ResponseWriter, _ *http.Request, token string) {
	s.mu.Lock()
	u := s.ensureBot(token).user
	s.mu.Unlock()
	writeOK(w, u)
}

func (s *Server) handleGetUpdates(w http.ResponseWriter, r *http.Request, token string) {
	params := parseParams(r)
	offset, _ := strconv.ParseInt(params["offset"], 10, 64)
	timeoutSecs, _ := strconv.Atoi(params["timeout"])

	s.mu.Lock()
	st := s.ensureBot(token)

	// Advance past acknowledged updates.
	if offset > 0 {
		keep := st.queue[:0]
		for _, u := range st.queue {
			if u.UpdateID >= offset {
				keep = append(keep, u)
			}
		}
		st.queue = keep
	}

	pending := make([]update, len(st.queue))
	copy(pending, st.queue)

	wake := make(chan struct{}, 1)
	st.listeners = append(st.listeners, wake)
	s.mu.Unlock()

	// If no updates and timeout > 0, wait for a delivery signal.
	if len(pending) == 0 && timeoutSecs > 0 {
		waitDur := time.Duration(timeoutSecs) * time.Second
		if waitDur > 30*time.Second {
			waitDur = 30 * time.Second
		}
		select {
		case <-wake:
		case <-time.After(waitDur):
		case <-r.Context().Done():
		}

		s.mu.Lock()
		st2 := s.ensureBot(token)
		if offset > 0 {
			keep := st2.queue[:0]
			for _, u := range st2.queue {
				if u.UpdateID >= offset {
					keep = append(keep, u)
				}
			}
			st2.queue = keep
		}
		pending = make([]update, len(st2.queue))
		copy(pending, st2.queue)
		s.removeListener(st2, wake)
		s.mu.Unlock()
	} else {
		s.mu.Lock()
		s.removeListener(s.ensureBot(token), wake)
		s.mu.Unlock()
	}

	writeOK(w, pending)
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request, fromToken string) {
	params := parseParams(r)

	chatIDStr := params["chat_id"]
	text := params["text"]
	if chatIDStr == "" {
		writeError(w, http.StatusBadRequest, "missing chat_id")
		return
	}
	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chat_id")
		return
	}

	s.mu.Lock()
	fromBot := s.ensureBot(fromToken)
	senderUser := fromBot.user

	msgID := s.nextID.Add(1)
	updateID := s.nextID.Add(1)

	msg := message{
		MessageID: msgID,
		From:      &senderUser,
		Chat:      chat{ID: chatID, Type: "private"},
		Date:      time.Now().Unix(),
		Text:      text,
	}

	// Deliver the message to the bot whose user ID matches chatID.
	var listeners []chan struct{}
	for _, st := range s.bots {
		if st.user.ID == chatID {
			if len(st.queue) < MaxQueueDepth {
				st.queue = append(st.queue, update{
					UpdateID: updateID,
					Message:  &msg,
				})
			}
			listeners = append(listeners, st.listeners...)
			break
		}
	}
	s.mu.Unlock()

	// Signal any waiting getUpdates callers.
	for _, ch := range listeners {
		select {
		case ch <- struct{}{}:
		default:
		}
	}

	writeOK(w, msg)
}

// -----------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------

// ensureBot returns or creates the botState for token. Must be called with mu held.
func (s *Server) ensureBot(token string) *botState {
	if st, ok := s.bots[token]; ok {
		return st
	}
	id := s.nextUser.Add(1)
	username := fmt.Sprintf("bot_%d", id)
	st := &botState{
		user: botUser{
			ID:        id,
			IsBot:     true,
			FirstName: username,
			Username:  username,
		},
	}
	s.bots[token] = st
	return st
}

// removeListener removes wake from st.listeners. Must be called with mu held.
func (s *Server) removeListener(st *botState, wake chan struct{}) {
	for i, ch := range st.listeners {
		if ch == wake {
			st.listeners = append(st.listeners[:i], st.listeners[i+1:]...)
			return
		}
	}
}

// -----------------------------------------------------------------------
// Parameter parsing — handles both form-encoded and JSON bodies
// -----------------------------------------------------------------------

// parseParams extracts key-value parameters from the request.
// It supports:
//  1. application/x-www-form-urlencoded (used by go-telegram-bot-api/v5)
//  2. application/json (fallback for other clients)
//  3. Query-string parameters (for GET requests)
func parseParams(r *http.Request) map[string]string {
	out := make(map[string]string)

	// Always merge query string.
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}

	ct := r.Header.Get("Content-Type")

	switch {
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		if err := r.ParseForm(); err == nil {
			for k, vs := range r.Form {
				if len(vs) > 0 {
					out[k] = vs[0]
				}
			}
		}

	case strings.HasPrefix(ct, "application/json"):
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err == nil {
			for k, v := range m {
				out[k] = fmt.Sprintf("%v", v)
			}
		}

	default:
		// Try form first, then JSON.
		if err := r.ParseForm(); err == nil && len(r.Form) > 0 {
			for k, vs := range r.Form {
				if len(vs) > 0 {
					out[k] = vs[0]
				}
			}
		}
	}

	return out
}

// -----------------------------------------------------------------------
// Path parsing
// -----------------------------------------------------------------------

// parsePath extracts (token, method) from a path like /bot<TOKEN>/method.
func parsePath(path string) (token, method string) {
	p := strings.TrimPrefix(path, "/")
	if !strings.HasPrefix(p, "bot") {
		return "", ""
	}
	p = p[len("bot"):]
	idx := strings.IndexByte(p, '/')
	if idx < 0 {
		return "", ""
	}
	token = p[:idx]
	method = p[idx+1:]
	if i := strings.IndexByte(method, '/'); i >= 0 {
		method = method[:i]
	}
	return token, method
}

// -----------------------------------------------------------------------
// JSON wire types (minimal subset of Telegram Bot API schema)
// -----------------------------------------------------------------------

type botUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type message struct {
	MessageID int64    `json:"message_id"`
	From      *botUser `json:"from,omitempty"`
	Chat      chat     `json:"chat"`
	Date      int64    `json:"date"`
	Text      string   `json:"text,omitempty"`
}

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message,omitempty"`
}

// -----------------------------------------------------------------------
// Response helpers
// -----------------------------------------------------------------------

type envelope struct {
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Desc   string `json:"description,omitempty"`
}

func writeOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envelope{OK: true, Result: result})
}

func writeError(w http.ResponseWriter, statusCode int, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(envelope{OK: false, Desc: desc})
}

// safePrefix returns the first 4 chars of the token for logging (not a secret).
func safePrefix(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[:4]
}

// noopWriter discards all writes.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
