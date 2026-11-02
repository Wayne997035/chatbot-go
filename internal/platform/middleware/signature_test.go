package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestValidateHMAC(t *testing.T) {
	secret := "test-secret-key"
	body := []byte(`{"events":[]}`)

	// 計算正確簽名
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name      string
		body      []byte
		signature string
		secret    string
		want      bool
	}{
		{"valid signature", body, validSig, secret, true},
		{"wrong signature", body, "wrong-signature", secret, false},
		{"wrong secret", body, validSig, "wrong-secret", false},
		{"empty body", []byte{}, validSig, secret, false},
		{"empty signature", body, "", secret, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateHMAC(tt.body, tt.signature, tt.secret)
			if got != tt.want {
				t.Errorf("validateHMAC() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecordLineWebhookBodyRejectsReplayWithLocalCache(t *testing.T) {
	originalCache := lineReplayCache
	lineReplayCache = newLocalLineReplayCache()
	t.Cleanup(func() {
		lineReplayCache = originalCache
	})

	body := []byte(`{"events":[{"webhookEventId":"01TEST"}]}`)
	if err := recordLineWebhookBody(context.Background(), nil, body); err != nil {
		t.Fatalf("recordLineWebhookBody() first call error = %v", err)
	}
	if err := recordLineWebhookBody(context.Background(), nil, body); !errors.Is(err, errLineWebhookReplay) {
		t.Fatalf("recordLineWebhookBody() second call error = %v, want replay", err)
	}
}

// Redis 中途斷線時不能拒收 LINE webhook（LINE 預設不重送，拒收就是訊息遺失）；改用記憶體防重放.
func TestReplayCheckFallsBackToMemoryWhenRedisFails(t *testing.T) {
	originalCache := lineReplayCache
	lineReplayCache = newLocalLineReplayCache()
	t.Cleanup(func() { lineReplayCache = originalCache })

	// 連到沒有服務的 port：每次操作都會失敗.
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = down.Close() })

	body := []byte(`{"events":[{"webhookEventId":"01DOWN"}]}`)
	if err := recordLineWebhookBody(context.Background(), down, body); err != nil {
		t.Fatalf("a Redis failure must not reject the webhook: %v", err)
	}
	if err := recordLineWebhookBody(context.Background(), down, body); !errors.Is(err, errLineWebhookReplay) {
		t.Errorf("replays are still caught in memory, got %v", err)
	}
}

func TestReadLineWebhookBody(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantLen int64
		wantErr error
	}{
		{
			name:    "within limit",
			body:    `{"events":[]}`,
			wantLen: int64(len(`{"events":[]}`)),
		},
		{
			name:    "exactly at limit",
			body:    strings.Repeat("a", int(maxLineWebhookBodyBytes)),
			wantLen: maxLineWebhookBodyBytes,
		},
		{
			name:    "over limit",
			body:    strings.Repeat("a", int(maxLineWebhookBodyBytes)+1),
			wantErr: errLineWebhookBodyTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readLineWebhookBody(strings.NewReader(tt.body))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("readLineWebhookBody() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readLineWebhookBody() error = %v", err)
			}
			if int64(len(got)) != tt.wantLen {
				t.Fatalf("readLineWebhookBody() len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}
