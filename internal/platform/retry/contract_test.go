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

// typeIs pins a value type at compile time: when the field type changes this call stops compiling.
func typeIs[T any](_ T) {}

func TestPolicyFieldsContract(t *testing.T) {
	p := retry.Policy{
		MaxAttempts:   1,
		Backoff:       func(int) time.Duration { return 0 },
		Sleep:         func(context.Context, time.Duration) error { return nil },
		Retryable:     func(error) bool { return false },
		BeforeAttempt: func(context.Context, int) error { return nil },
	}
	typeIs[int](p.MaxAttempts)
	typeIs[func(int) time.Duration](p.Backoff)
	typeIs[func(context.Context, time.Duration) error](p.Sleep)
	typeIs[func(error) bool](p.Retryable)
	typeIs[func(context.Context, int) error](p.BeforeAttempt)
}
