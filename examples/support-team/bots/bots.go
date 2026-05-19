// Package bots implements the five deterministic bots in the support-team demo.
//
// All bots are constructed with a proxy endpoint so every Bot API call flows
// through b2bdbg. None of them make external network calls; they operate on
// scripted, in-memory logic so the demo is fully reproducible offline.
//
// Conversation flow for a support request:
//
//	Customer task → Router bot
//	  Router classifies: sales / order / refund
//	  Router → specialist (Sales | Order | Refund)
//	  Specialist replies to Router
//	  If refund amount > threshold → Router → HumanApprove → Router
//	  Router aggregates and replies to customer
//
// Loop scenario: the Router sends the same refund task twice (simulating a
// retry bug), which the proxy's loop detector flags as b2b.loop.depth > 0.
package bots

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// RefundThreshold is the USD amount above which a refund requires human approval.
const RefundThreshold = 100

// -----------------------------------------------------------------------
// Bot construction helpers
// -----------------------------------------------------------------------

// newBot creates a tgbotapi.BotAPI that routes through the given proxy endpoint.
// The endpoint must be a base URL like "http://host:port" with no trailing slash.
// The bot API library appends the token and method name via the format string.
func newBot(token, proxyEndpoint string) (*tgbotapi.BotAPI, error) {
	// tgbotapi.NewBotAPIWithAPIEndpoint expects a URL template with two %s
	// substitutions: the first for the token, the second for the method name.
	apiURL := proxyEndpoint + "/bot%s/%s"
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint(token, apiURL)
	if err != nil {
		return nil, fmt.Errorf("bots: %w", err)
	}
	bot.Debug = false
	return bot, nil
}

// ensureLogger returns l if non-nil, otherwise a discard logger.
func ensureLogger(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// -----------------------------------------------------------------------
// RouterBot
// -----------------------------------------------------------------------

// RouterBot receives customer tasks, classifies them, dispatches to specialist
// bots, optionally routes high-value refunds through HumanApprove, aggregates
// responses, and signals completion.
type RouterBot struct {
	bot       *tgbotapi.BotAPI
	log       *slog.Logger
	salesID   int64 // user IDs of specialist bots (= their "chat IDs" for DMs)
	orderID   int64
	refundID  int64
	approveID int64

	mu      sync.Mutex
	pending map[int]chan string // Telegram message ID → reply channel
}

// NewRouterBot constructs a RouterBot that dispatches to the given specialist
// bot IDs. All Bot API calls flow through proxyEndpoint. logger may be nil.
func NewRouterBot(
	token string,
	proxyEndpoint string,
	salesID, orderID, refundID, approveID int64,
	logger *slog.Logger,
) (*RouterBot, error) {
	bot, err := newBot(token, proxyEndpoint)
	if err != nil {
		return nil, err
	}
	return &RouterBot{
		bot:       bot,
		log:       ensureLogger(logger),
		salesID:   salesID,
		orderID:   orderID,
		refundID:  refundID,
		approveID: approveID,
		pending:   make(map[int]chan string),
	}, nil
}

// SelfID returns the Telegram user ID of this bot.
func (r *RouterBot) SelfID() int64 { return r.bot.Self.ID }

// Run processes updates until ctx is cancelled. It must be called in its own
// goroutine because it blocks on getUpdates.
// If ready is non-nil, it is closed once the first getUpdates poll is issued.
func (r *RouterBot) Run(ctx context.Context, ready chan<- struct{}) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 1
	updates := r.bot.GetUpdatesChan(u)

	if ready != nil {
		close(ready)
	}

	for {
		select {
		case <-ctx.Done():
			r.bot.StopReceivingUpdates()
			return
		case upd, ok := <-updates:
			if !ok {
				return
			}
			if upd.Message == nil {
				continue
			}
			r.handleIncomingReply(upd.Message)
		}
	}
}

// HandleTask dispatches a customer task and blocks until a response is
// collected or ctx is cancelled. It returns the aggregated reply text.
func (r *RouterBot) HandleTask(ctx context.Context, customerChatID int64, task string) (string, error) {
	// Route task to specialist.
	specialist, specialistName := r.classify(task)
	r.log.Info(
		"router: classifying task",
		slog.String("task", task),
		slog.String("specialist", specialistName),
	)

	// Create a reply channel before sending (to avoid race with fast replies).
	replyCh := make(chan string, 1)

	// Send to specialist.
	msg := tgbotapi.NewMessage(specialist, task)
	sent, err := r.bot.Send(msg)
	if err != nil {
		return "", fmt.Errorf("router: send to %s: %w", specialistName, err)
	}

	r.mu.Lock()
	r.pending[sent.MessageID] = replyCh
	r.mu.Unlock()

	// Trigger a loop detection scenario: for refund tasks, resend the same
	// message to the specialist. The proxy will flag this as a loop because
	// the same (from, to, text-hash) tuple repeats within the loop window.
	if specialistName == "refund" {
		dupMsg := tgbotapi.NewMessage(specialist, task)
		_, _ = r.bot.Send(dupMsg)
	}

	// Wait for the specialist to reply via handleIncomingReply → replyCh.
	var reply string
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case reply = <-replyCh:
	}

	// If refund exceeds threshold, route through HumanApprove.
	if specialistName == "refund" && strings.Contains(reply, "USD") {
		amount := parseAmount(reply)
		if amount > RefundThreshold {
			r.log.Info(
				"router: requesting human approval",
				slog.Float64("amount", amount),
			)
			approveCh := make(chan string, 1)
			approveMsg := tgbotapi.NewMessage(r.approveID, fmt.Sprintf("approve refund %s", reply))
			sentApprove, err2 := r.bot.Send(approveMsg)
			if err2 != nil {
				return "", fmt.Errorf("router: send to human-approve: %w", err2)
			}

			r.mu.Lock()
			r.pending[sentApprove.MessageID] = approveCh
			r.mu.Unlock()

			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case approveReply := <-approveCh:
				reply = approveReply
			}
		}
	}

	// Notify original customer.
	final := fmt.Sprintf("Support response: %s", reply)
	if _, err := r.bot.Send(tgbotapi.NewMessage(customerChatID, final)); err != nil {
		r.log.Warn("router: could not notify customer", slog.Any("error", err))
	}

	return final, nil
}

// handleIncomingReply dispatches an incoming message to the waiting channel
// for the pending task. Called by Run in the getUpdates goroutine.
func (r *RouterBot) handleIncomingReply(msg *tgbotapi.Message) {
	r.log.Info(
		"router: received reply",
		slog.Int64("from", msg.From.ID),
		slog.String("text", msg.Text),
	)

	r.mu.Lock()
	// Take the first pending channel (demo has one task in flight at a time).
	var ch chan string
	for msgID, c := range r.pending {
		ch = c
		delete(r.pending, msgID)
		break
	}
	r.mu.Unlock()

	if ch != nil {
		select {
		case ch <- msg.Text:
		default:
		}
	}
}

// classify returns (targetBotID, specialistName) for the task text.
func (r *RouterBot) classify(task string) (int64, string) {
	lower := strings.ToLower(task)
	switch {
	case strings.Contains(lower, "refund"):
		return r.refundID, "refund"
	case strings.Contains(lower, "order"):
		return r.orderID, "order"
	default:
		return r.salesID, "sales"
	}
}

// -----------------------------------------------------------------------
// SalesBot
// -----------------------------------------------------------------------

// SalesBot handles sales enquiries with deterministic responses.
type SalesBot struct {
	bot *tgbotapi.BotAPI
	log *slog.Logger
}

// NewSalesBot constructs a SalesBot pointed at the proxy. logger may be nil.
func NewSalesBot(token, proxyEndpoint string, logger *slog.Logger) (*SalesBot, error) {
	bot, err := newBot(token, proxyEndpoint)
	if err != nil {
		return nil, err
	}
	return &SalesBot{bot: bot, log: ensureLogger(logger)}, nil
}

// SelfID returns the Telegram user ID of this bot.
func (b *SalesBot) SelfID() int64 { return b.bot.Self.ID }

// Run processes updates until ctx is cancelled.
// If ready is non-nil, it is closed once the first getUpdates poll is issued.
func (b *SalesBot) Run(ctx context.Context, ready chan<- struct{}) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 1
	updates := b.bot.GetUpdatesChan(u)
	if ready != nil {
		close(ready)
	}
	for {
		select {
		case <-ctx.Done():
			b.bot.StopReceivingUpdates()
			return
		case upd, ok := <-updates:
			if !ok {
				return
			}
			if upd.Message == nil {
				continue
			}
			b.handleMessage(upd.Message)
		}
	}
}

func (b *SalesBot) handleMessage(msg *tgbotapi.Message) {
	b.log.Info("sales: handling", slog.String("text", msg.Text))
	reply := fmt.Sprintf("Sales answer: our pricing starts at $9.99/month for %q", msg.Text)
	// Reply to the sender (From.ID), not the chat (Chat.ID).
	// In the mock, a bot-to-bot DM arrives with Chat.ID == self; the sender is From.ID.
	replyTo := msg.Chat.ID
	if msg.From != nil && msg.From.ID != msg.Chat.ID {
		replyTo = msg.From.ID
	}
	if _, err := b.bot.Send(tgbotapi.NewMessage(replyTo, reply)); err != nil {
		b.log.Warn("sales: send reply", slog.Any("error", err))
	}
}

// -----------------------------------------------------------------------
// OrderBot
// -----------------------------------------------------------------------

// OrderBot handles order status enquiries.
type OrderBot struct {
	bot *tgbotapi.BotAPI
	log *slog.Logger
}

// NewOrderBot constructs an OrderBot pointed at the proxy. logger may be nil.
func NewOrderBot(token, proxyEndpoint string, logger *slog.Logger) (*OrderBot, error) {
	bot, err := newBot(token, proxyEndpoint)
	if err != nil {
		return nil, err
	}
	return &OrderBot{bot: bot, log: ensureLogger(logger)}, nil
}

// SelfID returns the Telegram user ID of this bot.
func (b *OrderBot) SelfID() int64 { return b.bot.Self.ID }

// Run processes updates until ctx is cancelled.
// If ready is non-nil, it is closed once the first getUpdates poll is issued.
func (b *OrderBot) Run(ctx context.Context, ready chan<- struct{}) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 1
	updates := b.bot.GetUpdatesChan(u)
	if ready != nil {
		close(ready)
	}
	for {
		select {
		case <-ctx.Done():
			b.bot.StopReceivingUpdates()
			return
		case upd, ok := <-updates:
			if !ok {
				return
			}
			if upd.Message == nil {
				continue
			}
			b.handleMessage(upd.Message)
		}
	}
}

func (b *OrderBot) handleMessage(msg *tgbotapi.Message) {
	b.log.Info("order: handling", slog.String("text", msg.Text))
	reply := fmt.Sprintf("Order status for %q: shipped, arriving in 2 business days", msg.Text)
	replyTo := msg.Chat.ID
	if msg.From != nil && msg.From.ID != msg.Chat.ID {
		replyTo = msg.From.ID
	}
	if _, err := b.bot.Send(tgbotapi.NewMessage(replyTo, reply)); err != nil {
		b.log.Warn("order: send reply", slog.Any("error", err))
	}
}

// -----------------------------------------------------------------------
// RefundBot
// -----------------------------------------------------------------------

// RefundBot handles refund requests. For requests containing a dollar amount
// above RefundThreshold it echoes the amount so the Router can escalate to
// HumanApprove.
type RefundBot struct {
	bot *tgbotapi.BotAPI
	log *slog.Logger
}

// NewRefundBot constructs a RefundBot pointed at the proxy. logger may be nil.
func NewRefundBot(token, proxyEndpoint string, logger *slog.Logger) (*RefundBot, error) {
	bot, err := newBot(token, proxyEndpoint)
	if err != nil {
		return nil, err
	}
	return &RefundBot{bot: bot, log: ensureLogger(logger)}, nil
}

// SelfID returns the Telegram user ID of this bot.
func (b *RefundBot) SelfID() int64 { return b.bot.Self.ID }

// Run processes updates until ctx is cancelled.
// If ready is non-nil, it is closed once the first getUpdates poll is issued.
func (b *RefundBot) Run(ctx context.Context, ready chan<- struct{}) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 1
	updates := b.bot.GetUpdatesChan(u)
	if ready != nil {
		close(ready)
	}
	for {
		select {
		case <-ctx.Done():
			b.bot.StopReceivingUpdates()
			return
		case upd, ok := <-updates:
			if !ok {
				return
			}
			if upd.Message == nil {
				continue
			}
			b.handleMessage(upd.Message)
		}
	}
}

func (b *RefundBot) handleMessage(msg *tgbotapi.Message) {
	b.log.Info("refund: handling", slog.String("text", msg.Text))
	// Extract amount from task text; default to 150 USD so the approval path fires.
	amount := parseAmount(msg.Text)
	if amount == 0 {
		amount = 150
	}
	reply := fmt.Sprintf("Refund processed: %.0f USD for %q", amount, msg.Text)
	replyTo := msg.Chat.ID
	if msg.From != nil && msg.From.ID != msg.Chat.ID {
		replyTo = msg.From.ID
	}
	if _, err := b.bot.Send(tgbotapi.NewMessage(replyTo, reply)); err != nil {
		b.log.Warn("refund: send reply", slog.Any("error", err))
	}
}

// -----------------------------------------------------------------------
// HumanApproveBot
// -----------------------------------------------------------------------

// HumanApproveBot simulates a human-in-the-loop approval gate. In the demo
// it always approves automatically, but the span still appears in the trace
// as a distinct hop, demonstrating the full multi-bot chain.
type HumanApproveBot struct {
	bot *tgbotapi.BotAPI
	log *slog.Logger
}

// NewHumanApproveBot constructs a HumanApproveBot pointed at the proxy.
// logger may be nil.
func NewHumanApproveBot(token, proxyEndpoint string, logger *slog.Logger) (*HumanApproveBot, error) {
	bot, err := newBot(token, proxyEndpoint)
	if err != nil {
		return nil, err
	}
	return &HumanApproveBot{bot: bot, log: ensureLogger(logger)}, nil
}

// SelfID returns the Telegram user ID of this bot.
func (b *HumanApproveBot) SelfID() int64 { return b.bot.Self.ID }

// Run processes updates until ctx is cancelled.
// If ready is non-nil, it is closed once the first getUpdates poll is issued.
func (b *HumanApproveBot) Run(ctx context.Context, ready chan<- struct{}) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 1
	updates := b.bot.GetUpdatesChan(u)
	if ready != nil {
		close(ready)
	}
	for {
		select {
		case <-ctx.Done():
			b.bot.StopReceivingUpdates()
			return
		case upd, ok := <-updates:
			if !ok {
				return
			}
			if upd.Message == nil {
				continue
			}
			b.handleMessage(upd.Message)
		}
	}
}

func (b *HumanApproveBot) handleMessage(msg *tgbotapi.Message) {
	b.log.Info("human-approve: reviewing", slog.String("text", msg.Text))
	reply := fmt.Sprintf("APPROVED: %s", msg.Text)
	replyTo := msg.Chat.ID
	if msg.From != nil && msg.From.ID != msg.Chat.ID {
		replyTo = msg.From.ID
	}
	if _, err := b.bot.Send(tgbotapi.NewMessage(replyTo, reply)); err != nil {
		b.log.Warn("human-approve: send reply", slog.Any("error", err))
	}
}

// -----------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------

// parseAmount tries to find a numeric dollar amount in the text
// (e.g. "$150" or "150 USD"). Returns 0 if none is found.
func parseAmount(text string) float64 {
	text = strings.ReplaceAll(text, "$", " ")
	for _, word := range strings.Fields(text) {
		word = strings.Trim(word, ".,;:")
		if v, err := strconv.ParseFloat(word, 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}
