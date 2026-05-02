package webhook

import (
	"chatbot-go/internal/platform/config"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReplyTextDoesNotPanicWithShortReplyTokenOnNonOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	originalHTTPClient := httpClient
	originalGetConfig := getConfig
	t.Cleanup(func() {
		httpClient = originalHTTPClient
		getConfig = originalGetConfig
	})

	httpClient = server.Client()
	getConfig = func() *config.Config {
		return &config.Config{
			Line: config.LineConfig{
				ChannelToken: "test-token",
				ReplyURL:     server.URL,
			},
		}
	}

	if err := ReplyText(context.Background(), "short", "hello"); err != nil {
		t.Fatalf("ReplyText() error = %v", err)
	}
}

func TestMaskedReplyToken(t *testing.T) {
	tests := []struct {
		name       string
		replyToken string
		want       string
	}{
		{"empty", "", ""},
		{"short", "abc", "abc"},
		{"exact prefix length", "12345678", "12345678"},
		{"long", "123456789", "12345678..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskedReplyToken(tt.replyToken); got != tt.want {
				t.Fatalf("maskedReplyToken() = %q, want %q", got, tt.want)
			}
		})
	}
}
