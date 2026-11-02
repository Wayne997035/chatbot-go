package webhook

import (
	"bytes"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/weather"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// blockingGeocoder 查地點時卡住，直到測試放行（模擬處理中的訊息）.
type blockingGeocoder struct{ started, release chan struct{} }

func (g *blockingGeocoder) Geocode(ctx context.Context, _ string) ([]geocoding.GeocodeResult, error) {
	close(g.started)
	select {
	case <-g.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func postWebhook(t *testing.T, h *WebhookHandler, body []byte) int {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	if err := h.HandleWebhook(echo.New().NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	return rec.Code
}

// 關機時處理到一半的訊息要做完、使用者有收到回覆；關機開始後不再接新的.
func TestShutdownFinishesMessagesBeingProcessed(t *testing.T) {
	api := newLineAPI(t, nil)
	geo := &blockingGeocoder{started: make(chan struct{}), release: make(chan struct{})}
	h := NewWebhookHandlerWithOptions(api.client, newFakeUserRepo(), weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil,
		conversation.NewMemoryManager(), geo, nil)

	if code := postWebhook(t, h, webhookBody(t, textEvent(fromUser, "某個小地方"))); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	<-geo.started

	done := make(chan error, 1)
	go func() { done <- h.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("shutdown returned while a message was still being processed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(geo.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(api.replies) != 1 {
		t.Fatalf("the user must get a reply before shutdown completes, got %d", len(api.replies))
	}

	if code := postWebhook(t, h, webhookBody(t, textEvent(fromUser, "說明"))); code != http.StatusServiceUnavailable {
		t.Errorf("after shutdown: status %d, want 503 so LINE can redeliver", code)
	}
	time.Sleep(20 * time.Millisecond)
	if len(api.replies) != 1 {
		t.Error("nothing is processed after shutdown")
	}
}

// 關機等太久：取消處理中的工作（例如卡住的外部呼叫），不會讓程式關不掉.
func TestShutdownTimeoutCancelsStuckMessages(t *testing.T) {
	api := newLineAPI(t, nil)
	geo := &blockingGeocoder{started: make(chan struct{}), release: make(chan struct{})}
	h := NewWebhookHandlerWithOptions(api.client, newFakeUserRepo(), weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil,
		conversation.NewMemoryManager(), geo, nil)
	postWebhook(t, h, webhookBody(t, textEvent(fromUser, "某個小地方")))
	<-geo.started

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.Shutdown(ctx); err == nil {
		t.Error("a timed-out shutdown reports it")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("the stuck call must be canceled, waited %v", waited)
	}
}
