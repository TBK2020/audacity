// Package notify fans service transitions out to Telegram / generic webhooks.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"labdeck/internal/config"
	"labdeck/internal/engine"
)

type Notifier interface {
	Notify(t engine.Transition) error
	Name() string
}

func Build(cfgs []config.Notifier) []Notifier {
	var out []Notifier
	for _, c := range cfgs {
		switch c.Type {
		case "telegram":
			if c.Token == "" || c.ChatID == "" {
				slog.Warn("telegram notifier missing token/chat_id, skipping")
				continue
			}
			out = append(out, &telegram{token: c.Token, chatID: c.ChatID})
		case "webhook":
			if c.URL == "" {
				slog.Warn("webhook notifier missing url, skipping")
				continue
			}
			out = append(out, &webhook{url: c.URL})
		default:
			slog.Warn("unknown notifier type", "type", c.Type)
		}
	}
	return out
}

// Dispatch sends t to every notifier without blocking the engine.
// Pending->Up transitions are informational noise on startup and are skipped.
func Dispatch(notifiers []Notifier, t engine.Transition) {
	if t.From == engine.StatusPending && (t.To == engine.StatusUp || t.To == engine.StatusDegraded) {
		return
	}
	for _, n := range notifiers {
		go func(n Notifier) {
			if err := n.Notify(t); err != nil {
				slog.Error("notify failed", "notifier", n.Name(), "err", err)
			}
		}(n)
	}
}

var client = &http.Client{Timeout: 10 * time.Second}

type telegram struct{ token, chatID string }

func (tg *telegram) Name() string { return "telegram" }

func (tg *telegram) Notify(t engine.Transition) error {
	emoji := map[engine.Status]string{
		engine.StatusUp:       "✅",
		engine.StatusDegraded: "⚠️",
		engine.StatusDown:     "🔴",
	}[t.To]
	text := fmt.Sprintf("%s %s: %s → %s\n%s\n%s",
		emoji, t.ServiceName, t.From, t.To,
		t.Reason, t.Time.Format("2006-01-02 15:04:05"))
	resp, err := client.PostForm(
		"https://api.telegram.org/bot"+tg.token+"/sendMessage",
		url.Values{"chat_id": {tg.chatID}, "text": {text}},
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram status %d", resp.StatusCode)
	}
	return nil
}

type webhook struct{ url string }

func (w *webhook) Name() string { return "webhook" }

func (w *webhook) Notify(t engine.Transition) error {
	payload, err := json.Marshal(map[string]any{
		"service_id":   t.ServiceID,
		"service_name": t.ServiceName,
		"from":         t.From,
		"to":           t.To,
		"reason":       t.Reason,
		"time":         t.Time.Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	resp, err := client.Post(w.url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}
