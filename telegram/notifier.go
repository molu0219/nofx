package telegram

import (
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"nofx/logger"
	"nofx/store"
)

// Notifier delivers operator-attention events (currently auto-pause) to the
// Telegram chat bound via Settings → Telegram. It implements the
// trader.Notifier interface without forcing the trader package to import
// tgbotapi.
//
// Each call opens a new Bot API client; this is a once-per-event path
// (auto-pause should be rare) so the overhead is fine and avoids sharing
// state with the long-lived interactive bot in bot.go.
type Notifier struct {
	store *store.Store
}

// NewNotifier wires the notifier to the framework store, which holds both
// the bot token and the chat-id of the bound user.
func NewNotifier(st *store.Store) *Notifier {
	return &Notifier{store: st}
}

// NotifyAutoPause sends a Markdown-formatted alert describing the auto-pause
// to the bound chat. No-op when the bot or chat is unconfigured.
func (n *Notifier) NotifyAutoPause(traderID, traderName, reason string) {
	if n == nil || n.store == nil {
		return
	}
	cfg, err := n.store.TelegramConfig().Get()
	if err != nil || cfg == nil || cfg.BotToken == "" || cfg.ChatID == 0 {
		// Not configured; silent no-op so log noise stays low for users who
		// haven't set up Telegram.
		return
	}

	bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		logger.Warnf("⛔ telegram notifier: bot init failed: %v", err)
		return
	}

	body := fmt.Sprintf(
		"⛔ *Trader auto-paused*\n\n*%s* (`%s`) has stopped itself.\n\n*Reason:* %s\n\nReview the dashboard, fix the root cause, then restart manually.",
		escapeMarkdown(traderName),
		escapeMarkdown(traderID),
		escapeMarkdown(reason),
	)
	msg := tgbotapi.NewMessage(cfg.ChatID, body)
	msg.ParseMode = tgbotapi.ModeMarkdown
	if _, err := bot.Send(msg); err != nil {
		logger.Warnf("⛔ telegram notifier: send failed: %v", err)
	}
}

// escapeMarkdown escapes the small set of Telegram Markdown (legacy mode)
// metacharacters that would otherwise corrupt the formatting. We deliberately
// use legacy Markdown (not MarkdownV2) because it's lenient about most punctuation.
func escapeMarkdown(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '*', '_', '`', '[':
			out = append(out, '\\', c)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
