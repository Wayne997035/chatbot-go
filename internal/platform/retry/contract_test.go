package retry_test

import (
	"chatbot-go/internal/platform/retry"
	"context"
	"testing"
	"time"
)

// F30-30 契約：簽名改一個字就編譯失敗.
var (
	_ func(context.Context, retry.Policy, func(context.Context, int) error) (int, error) = retry.Do
	_ func(context.Context, time.Duration) error                                         = retry.SleepCtx
)

func TestPolicyFieldsContract(t *testing.T) {
	p := retry.Policy{
		MaxAttempts:   1,
		Backoff:       func(int) time.Duration { return 0 },
		Sleep:         func(context.Context, time.Duration) error { return nil },
		Retryable:     func(error) bool { return false },
		BeforeAttempt: func(context.Context, int) error { return nil },
	}
	var (
		_ int                                        = p.MaxAttempts
		_ func(int) time.Duration                    = p.Backoff
		_ func(context.Context, time.Duration) error = p.Sleep
		_ func(error) bool                           = p.Retryable
		_ func(context.Context, int) error           = p.BeforeAttempt
	)
}
