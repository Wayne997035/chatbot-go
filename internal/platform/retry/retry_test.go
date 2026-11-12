package retry_test

import (
	"chatbot-go/internal/platform/retry"
	"context"
	"errors"
	"testing"
	"time"
)

var errTransient = errors.New("transient")

func alwaysRetry(error) bool { return true }

// F30-30.
func TestDoRetriesThenSucceeds(t *testing.T) {
	calls, sleeps := 0, 0
	p := retry.Policy{
		MaxAttempts: 2,
		Backoff:     func(int) time.Duration { return time.Second },
		Sleep:       func(context.Context, time.Duration) error { sleeps++; return nil },
		Retryable:   alwaysRetry,
	}
	attempts, err := retry.Do(context.Background(), p, func(_ context.Context, attempt int) error {
		calls++
		if attempt == 1 {
			return errTransient
		}
		return nil
	})
	if err != nil || attempts != 2 || calls != 2 || sleeps != 1 {
		t.Fatalf("err=%v attempts=%d calls=%d sleeps=%d", err, attempts, calls, sleeps)
	}
}

// F30-30.
func TestDoStopsOnNonRetryable(t *testing.T) {
	calls, sleeps := 0, 0
	p := retry.Policy{
		MaxAttempts: 3,
		Backoff:     func(int) time.Duration { return time.Second },
		Sleep:       func(context.Context, time.Duration) error { sleeps++; return nil },
		Retryable:   func(error) bool { return false },
	}
	attempts, err := retry.Do(context.Background(), p, func(context.Context, int) error {
		calls++
		return errTransient
	})
	if !errors.Is(err, errTransient) || attempts != 1 || calls != 1 || sleeps != 0 {
		t.Fatalf("err=%v attempts=%d calls=%d sleeps=%d", err, attempts, calls, sleeps)
	}
}

// F30-30：必須用 retry.SleepCtx 本身，取消後 100 毫秒內返回、fn 只呼叫 1 次.
func TestDoStopsWhenContextCanceledDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	p := retry.Policy{
		MaxAttempts: 3,
		Backoff:     func(int) time.Duration { return 5 * time.Second },
		Retryable:   alwaysRetry,
	}
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	attempts, err := retry.Do(ctx, p, func(context.Context, int) error {
		calls++
		return errTransient
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 100*time.Millisecond+20*time.Millisecond {
		t.Fatalf("returned after %v, want < 100ms after cancel", elapsed)
	}
	if calls != 1 || attempts != 1 {
		t.Fatalf("calls=%d attempts=%d, want 1", calls, attempts)
	}
}

// F30-30.
func TestDoMaxAttemptsOneAndThree(t *testing.T) {
	tests := []struct {
		name       string
		max        int
		wantCalls  int
		wantSleeps int
	}{
		{"one attempt never sleeps", 1, 1, 0},
		{"three attempts all fail", 3, 3, 2},
		{"zero treated as one", 0, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls, sleeps := 0, 0
			p := retry.Policy{
				MaxAttempts: tc.max,
				Backoff:     func(int) time.Duration { return time.Second },
				Sleep:       func(context.Context, time.Duration) error { sleeps++; return nil },
				Retryable:   alwaysRetry,
			}
			attempts, err := retry.Do(context.Background(), p, func(context.Context, int) error {
				calls++
				return errTransient
			})
			if !errors.Is(err, errTransient) || calls != tc.wantCalls || attempts != tc.wantCalls || sleeps != tc.wantSleeps {
				t.Fatalf("err=%v calls=%d attempts=%d sleeps=%d", err, calls, attempts, sleeps)
			}
		})
	}
}

// F30-30.
func TestDoBeforeAttemptErrorStops(t *testing.T) {
	errDenied := errors.New("denied")
	tests := []struct {
		name         string
		denyAt       int
		wantAttempts int
	}{
		{"denied first attempt", 1, 0},
		{"denied retry", 2, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := retry.Policy{
				MaxAttempts: 3,
				Sleep:       func(context.Context, time.Duration) error { return nil },
				Retryable:   alwaysRetry,
				BeforeAttempt: func(_ context.Context, attempt int) error {
					if attempt == tc.denyAt {
						return errDenied
					}
					return nil
				},
			}
			attempts, err := retry.Do(context.Background(), p, func(context.Context, int) error {
				calls++
				return errTransient
			})
			if !errors.Is(err, errDenied) || attempts != tc.wantAttempts || calls != tc.wantAttempts {
				t.Fatalf("err=%v attempts=%d calls=%d", err, attempts, calls)
			}
		})
	}
}

func TestSleepCtxReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := retry.SleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if err := retry.SleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("err = %v", err)
	}
}
