package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestHandleWebhookReturnsOKForEmptyEvents(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks", strings.NewReader(`{"events":[]}`))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	h := NewWebhookHandler(nil, nil, nil, nil)

	if err := h.HandleWebhook(c); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("HandleWebhook() status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHandleWebhookRejectsOversizedBody(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/webhooks",
		strings.NewReader(strings.Repeat("a", int(maxWebhookBodyBytes)+1)),
	)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	h := NewWebhookHandler(nil, nil, nil, nil)

	if err := h.HandleWebhook(c); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("HandleWebhook() status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}
