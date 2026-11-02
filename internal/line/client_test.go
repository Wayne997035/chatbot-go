package line

import (
	"chatbot-go/internal/models"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captured 記錄最後一次收到的 payload.
type captured struct{ payload map[string]any }

// fakeLINE 假的 LINE API；回傳指向它的 Client.
func fakeLINE(t *testing.T, status int, respBody string) (*Client, *captured) {
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
	return NewClient(&Config{Token: "test-token", ReplyURL: srv.URL, PushURL: srv.URL, LoadingURL: srv.URL}, nil), got
}

func TestReplyTextWithQuickReply(t *testing.T) {
	c, got := fakeLINE(t, http.StatusOK, "{}")
	if err := c.ReplyText(context.Background(), "tok", "hi", models.LocationItem("傳送位置"), models.MessageItem("說明", "說明")); err != nil {
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
	c, got := fakeLINE(t, http.StatusOK, "{}")
	if err := c.ReplyText(context.Background(), "tok", "hi"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got.payload)
	if strings.Contains(string(raw), "quickReply") {
		t.Errorf("unexpected quickReply: %s", raw)
	}
}

func TestPushAndLoadingPayloads(t *testing.T) {
	c, got := fakeLINE(t, http.StatusOK, "{}")
	if err := c.Push(context.Background(), "U1", "alert"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got.payload)
	if !strings.Contains(string(raw), `"to":"U1"`) || !strings.Contains(string(raw), `"text":"alert"`) {
		t.Errorf("push payload: %s", raw)
	}
	if err := c.StartLoading(context.Background(), "U1", 20); err != nil {
		t.Fatal(err)
	}
	if got.payload["chatId"] != "U1" || got.payload["loadingSeconds"] != float64(20) {
		t.Errorf("loading payload: %v", got.payload)
	}
}

func TestLINEErrorIsReturnedAndRedacted(t *testing.T) {
	c, _ := fakeLINE(t, http.StatusBadRequest, `{"message":"Invalid reply token","details":[{"property":"token=abcdef123"}]}`)
	err := c.ReplyText(context.Background(), "tok", "hi")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "Invalid reply token") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "abcdef123") {
		t.Errorf("token leaked: %v", err)
	}
	if err := c.Push(context.Background(), "U1", "alert"); err == nil {
		t.Error("push failure must be reported")
	}
}

// 回覆失敗要留下紀錄（以前 100 多個呼叫點都把錯誤丟掉，LINE 拒收訊息時完全看不到）；紀錄不能含 token.
func TestReplyFailureIsLogged(t *testing.T) {
	c, _ := fakeLINE(t, http.StatusBadRequest, `{"message":"Invalid reply token"}`)
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := c.ReplyText(context.Background(), "expired-token", "hi"); err == nil {
		t.Fatal("want an error")
	}
	out := buf.String()
	if !strings.Contains(out, "LINE reply failed") || !strings.Contains(out, "400") || strings.Contains(out, "test-token") {
		t.Errorf("log = %s", out)
	}
}

// contentServer 假的圖片下載 API.
func contentServer(t *testing.T, status int, body []byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/content/m1/content" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return NewClient(&Config{Token: "test-token", ContentURL: srv.URL + "/content/"}, nil)
}

var jpeg = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, make([]byte, 64)...)

func TestDownloadImage(t *testing.T) {
	data, mime, err := contentServer(t, http.StatusOK, jpeg).DownloadImage(context.Background(), "m1")
	if err != nil || mime != "image/jpeg" || len(data) != len(jpeg) {
		t.Fatalf("data=%d mime=%q err=%v", len(data), mime, err)
	}
}

func TestDownloadImageRejectsNonImagesAndHugeFilesAndErrors(t *testing.T) {
	if _, _, err := contentServer(t, http.StatusOK, []byte("<html>not an image</html>")).
		DownloadImage(context.Background(), "m1"); !errors.Is(err, ErrNotImage) {
		t.Errorf("html: %v", err)
	}
	huge := append(append([]byte{}, jpeg...), make([]byte, MaxImageBytes)...)
	if _, _, err := contentServer(t, http.StatusOK, huge).DownloadImage(context.Background(), "m1"); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Errorf("huge: %v", err)
	}
	if _, _, err := contentServer(t, http.StatusNotFound, nil).DownloadImage(context.Background(), "m1"); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
}
