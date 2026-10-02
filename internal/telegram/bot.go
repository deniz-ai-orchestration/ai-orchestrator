package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/quota"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// API is the part of the Bot API orch uses.
type API interface {
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
	Send(ctx context.Context, chatID int64, text string, buttons [][]Button, silent bool) error
	AnswerCallback(ctx context.Context, id, text string) error
}

// Stopper kills a task's running agent container.
type Stopper interface {
	Stop(taskID int64) bool
}

// Bot sends notifications and answers commands from Cfg.Telegram.UserID.
type Bot struct {
	API    API
	Store  *store.Store
	Cfg    *config.Config
	Quota  *quota.Tracker
	Agents Stopper
	Log    *slog.Logger
	// PollTimeout is the getUpdates long-poll timeout (default 50s).
	PollTimeout time.Duration
	// NotifyEvery is how often pending notifications are sent (default 5s).
	NotifyEvery time.Duration
}

// Run polls for commands and sends notifications until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.loop(ctx, b.NotifyEvery, 5*time.Second, b.NotifyOnce, "notify")
	}()
	b.loop(ctx, 0, 0, b.PollOnce, "poll")
	<-done
	return nil
}

func (b *Bot) loop(ctx context.Context, every, def time.Duration, f func(context.Context) error, name string) {
	if every <= 0 {
		every = def
	}
	for {
		wait := every
		if err := f(ctx); err != nil && ctx.Err() == nil {
			var re *RetryError
			if errors.As(err, &re) {
				wait = max(wait, re.After)
			} else {
				wait = max(wait, 5*time.Second)
			}
			b.Log.Warn("telegram "+name+" failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// PollOnce fetches and handles one batch of updates. The offset is saved
// after each update, so a restart never handles one twice.
func (b *Bot) PollOnce(ctx context.Context) error {
	v, err := b.Store.Setting(ctx, store.SettingTelegramOffset)
	if err != nil {
		return err
	}
	offset, _ := strconv.ParseInt(v, 10, 64)
	timeout := b.PollTimeout
	if timeout <= 0 {
		timeout = 50 * time.Second
	}
	ups, err := b.API.GetUpdates(ctx, offset, timeout)
	if err != nil {
		return err
	}
	for _, u := range ups {
		if err := b.handle(ctx, u); err != nil {
			b.Log.Error("telegram update failed", "update", u.UpdateID, "err", err)
		}
		if err := b.Store.SetSetting(ctx, store.SettingTelegramOffset, strconv.FormatInt(u.UpdateID+1, 10)); err != nil {
			return err
		}
	}
	return nil
}

// trusted reports whether the update comes from the configured user in a
// private chat. Everything else is ignored without a reply.
func (b *Bot) trusted(from *User, chat *Chat) bool {
	uid := b.Cfg.Telegram.UserID
	return uid != 0 && from != nil && from.ID == uid && (chat == nil || chat.ID == uid)
}

func (b *Bot) handle(ctx context.Context, u Update) error {
	switch {
	case u.Message != nil:
		m := u.Message
		if !b.trusted(m.From, &m.Chat) {
			b.Log.Warn("ignoring telegram message from an unknown user")
			return nil
		}
		reply := b.Command(ctx, m.Text)
		return b.API.Send(ctx, b.Cfg.Telegram.UserID, reply, nil, false)
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		var chat *Chat
		if q.Message != nil {
			chat = &q.Message.Chat
		}
		if !b.trusted(&q.From, chat) {
			return nil
		}
		toast, reply := b.press(ctx, q.Data)
		if err := b.API.AnswerCallback(ctx, q.ID, toast); err != nil {
			return err
		}
		if reply != "" {
			return b.API.Send(ctx, b.Cfg.Telegram.UserID, reply, nil, true)
		}
	}
	return nil
}

// press runs a button's single-use action.
func (b *Bot) press(ctx context.Context, data string) (toast, reply string) {
	nonce, ok := strings.CutPrefix(data, "a:")
	if !ok {
		return "Unknown button", ""
	}
	a, err := b.Store.UseAction(ctx, nonce)
	if errors.Is(err, store.ErrActionUsed) {
		return "That button was already used", ""
	}
	if err != nil {
		return "Failed: " + err.Error(), ""
	}
	switch a.Action {
	case "retry", "cancel":
		return "Done", b.Command(ctx, fmt.Sprintf("/%s %d", a.Action, a.TaskID))
	}
	return "Unknown action", ""
}

// NotifyOnce sends pending notifications in order. A send failure stops
// the batch so messages never arrive out of order.
func (b *Bot) NotifyOnce(ctx context.Context) error {
	items, err := b.Store.PendingEffectsOf(ctx, 20, engine.EffNotify)
	if err != nil {
		return err
	}
	for _, it := range items {
		t, err := b.Store.GetTask(ctx, it.TaskID)
		if err != nil {
			return err
		}
		text, loud, err := b.render(ctx, t, it.Effect.Arg)
		if err != nil {
			return err
		}
		var buttons [][]Button
		if it.Effect.Arg == "needs_human" {
			if buttons, err = b.holdButtons(ctx, t); err != nil {
				return err
			}
		}
		if err := b.API.Send(ctx, b.Cfg.Telegram.UserID, text, buttons, !loud); err != nil {
			if ferr := b.Store.FailEffect(ctx, it.ID, err); ferr != nil {
				return ferr
			}
			return err
		}
		if err := b.Store.CompleteEffect(ctx, it.ID); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) holdButtons(ctx context.Context, t store.Task) ([][]Button, error) {
	var row []Button
	for _, a := range []struct{ action, label string }{{"retry", "Retry"}, {"cancel", "Cancel task"}} {
		n, err := b.Store.CreateAction(ctx, store.Action{TaskID: t.ID, Action: a.action})
		if err != nil {
			return nil, err
		}
		row = append(row, Button{Text: a.label, CallbackData: "a:" + n})
	}
	return [][]Button{row}, nil
}

// RunNotifications only sends notifications, without polling for
// commands. With LogAPI it keeps the outbox drained when Telegram is off.
func (b *Bot) RunNotifications(ctx context.Context) error {
	b.loop(ctx, b.NotifyEvery, 5*time.Second, b.NotifyOnce, "notify")
	return nil
}

// LogAPI writes notifications to the log instead of Telegram.
type LogAPI struct{ Log *slog.Logger }

// GetUpdates never returns updates.
func (LogAPI) GetUpdates(ctx context.Context, _ int64, _ time.Duration) ([]Update, error) {
	<-ctx.Done()
	return nil, nil
}

// Send logs the message.
func (l LogAPI) Send(_ context.Context, _ int64, text string, _ [][]Button, silent bool) error {
	l.Log.Info("notification", "text", text, "silent", silent)
	return nil
}

// AnswerCallback does nothing.
func (LogAPI) AnswerCallback(context.Context, string, string) error { return nil }
