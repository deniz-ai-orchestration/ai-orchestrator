// Package telegram is orch's Telegram bot: notifications for task events
// and commands to steer orch, accepted from one user id only. It long-polls
// getUpdates, so PC2 needs no inbound port.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Update is one getUpdates entry.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Message is a chat message.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

// User is a Telegram account.
type User struct {
	ID int64 `json:"id"`
}

// Chat is where a message was sent.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// CallbackQuery is a press on an inline button.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// Button is an inline keyboard button.
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// Client calls the Bot API. The token is part of every URL, so errors are
// scrubbed before they leave the client.
type Client struct {
	BaseURL string
	token   string
	http    *http.Client
}

// New returns a client for the bot token.
func New(token string) *Client {
	return &Client{BaseURL: "https://api.telegram.org", token: token, http: &http.Client{Timeout: 70 * time.Second}}
}

func (c *Client) call(ctx context.Context, method string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return c.scrub(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return c.scrub(err)
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d: %w", method, resp.StatusCode, c.scrub(err))
	}
	if !env.OK {
		if env.Parameters.RetryAfter > 0 {
			return &RetryError{After: time.Duration(env.Parameters.RetryAfter) * time.Second}
		}
		return fmt.Errorf("telegram %s: %s", method, c.scrubText(env.Description))
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// RetryError is Telegram's flood control.
type RetryError struct{ After time.Duration }

func (e *RetryError) Error() string {
	return fmt.Sprintf("telegram flood control: retry after %s", e.After)
}

func (c *Client) scrub(err error) error {
	if err == nil || c.token == "" || !strings.Contains(err.Error(), c.token) {
		return err
	}
	return errors.New(c.scrubText(err.Error()))
}

func (c *Client) scrubText(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "<token>")
}

// GetUpdates long-polls for updates from offset on.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

// Send sends a plain-text message, optionally with buttons. silent sends it
// without a notification sound.
func (c *Client) Send(ctx context.Context, chatID int64, text string, buttons [][]Button, silent bool) error {
	in := map[string]any{"chat_id": chatID, "text": text, "disable_notification": silent,
		"link_preview_options": map[string]bool{"is_disabled": true}}
	if len(buttons) > 0 {
		in["reply_markup"] = map[string]any{"inline_keyboard": buttons}
	}
	return c.call(ctx, "sendMessage", in, nil)
}

// AnswerCallback acknowledges a button press with a short toast.
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
