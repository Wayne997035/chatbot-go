package webhook

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captured 記錄最後一次收到的 payload.
type captured struct{ payload map[string]any }

// lineServer 假的 LINE API.
func lineServer(t *testing.T, status int, respBody string) *captured {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		data, _ := io.ReadAll(r.Body)
		got.payload = map[string]any{}
		_ = json.Unmarshal(data, &got.payload)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	prev := config.Get()
	config.Set(&config.Config{Line: config.LineConfig{ChannelToken: "test-token", ReplyURL: srv.URL, PushURL: srv.URL}})
	t.Cleanup(func() { config.Set(prev) })
	return got
}

func TestReplyTextWithQuickReply(t *testing.T) {
	got := lineServer(t, http.StatusOK, "{}")
	if err := ReplyText(context.Background(), "tok", "hi", models.LocationItem("傳送位置"), models.MessageItem("說明", "說明")); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got.payload)
	for _, want := range []string{`"replyToken":"tok"`, `"text":"hi"`, `"type":"location"`, `"label":"傳送位置"`, `"text":"說明"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload missing %s: %s", want, raw)
		}
	}
}

func TestReplyTextWithoutQuickReplyOmitsField(t *testing.T) {
	got := lineServer(t, http.StatusOK, "{}")
	if err := ReplyText(context.Background(), "tok", "hi"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got.payload)
	if strings.Contains(string(raw), "quickReply") {
		t.Errorf("unexpected quickReply: %s", raw)
	}
}

func TestLINEErrorIsReturnedAndRedacted(t *testing.T) {
	lineServer(t, http.StatusBadRequest, `{"message":"Invalid reply token","details":[{"property":"token=abcdef123"}]}`)
	err := ReplyText(context.Background(), "tok", "hi")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "Invalid reply token") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "abcdef123") {
		t.Errorf("token leaked: %v", err)
	}
	if err := PushText(context.Background(), "U1", "alert"); err == nil {
		t.Error("push failure must be reported")
	}
}

func TestHelpAliases(t *testing.T) {
	for _, alias := range []string{"說明", "help", "HELP", "功能", "？"} {
		if !helpAliases[strings.ToLower(alias)] {
			t.Errorf("%q should show help", alias)
		}
	}
}

// 回覆失敗要留下紀錄（以前 100 多個呼叫點都把錯誤丟掉，LINE 拒收訊息時完全看不到）；紀錄不能含 token.
func TestReplyFailureIsLogged(t *testing.T) {
	lineServer(t, http.StatusBadRequest, `{"message":"Invalid reply token"}`)
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := ReplyText(context.Background(), "expired-token", "hi"); err == nil {
		t.Fatal("want an error")
	}
	out := buf.String()
	if !strings.Contains(out, "LINE reply failed") || !strings.Contains(out, "400") || strings.Contains(out, "test-token") {
		t.Errorf("log = %s", out)
	}
}
