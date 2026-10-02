package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		switch r.URL.Path {
		case "/bot123:SECRET/getUpdates":
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":7,"message":{"message_id":1,"from":{"id":4242},"chat":{"id":4242,"type":"private"},"text":"/status"}}]}`))
		case "/bot123:SECRET/sendMessage":
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Not Found"}`))
		}
	}))
	defer srv.Close()
	c := New("123:SECRET")
	c.BaseURL = srv.URL
	ctx := context.Background()

	ups, err := c.GetUpdates(ctx, 5, 30*time.Second)
	if err != nil || len(ups) != 1 || ups[0].Message.Text != "/status" || ups[0].Message.From.ID != 4242 {
		t.Fatalf("updates %+v err %v", ups, err)
	}
	if got["offset"] != float64(5) || got["timeout"] != float64(30) {
		t.Fatalf("request %v", got)
	}
	err = c.Send(ctx, 4242, "hi", [][]Button{{{Text: "Retry", CallbackData: "a:x"}}}, true)
	var re *RetryError
	if !errors.As(err, &re) || re.After != 3*time.Second {
		t.Fatalf("flood control: %v", err)
	}
	if got["disable_notification"] != true || got["reply_markup"] == nil {
		t.Fatalf("send request %v", got)
	}
	if err := c.AnswerCallback(ctx, "q", "ok"); err == nil || !strings.Contains(err.Error(), "Not Found") {
		t.Fatalf("answer: %v", err)
	}

	// Transport errors carry the URL; the token must not survive.
	c.BaseURL = "http://127.0.0.1:1"
	_, err = c.GetUpdates(ctx, 0, time.Second)
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "<token>") {
		t.Fatalf("unscrubbed error: %v", err)
	}
}
