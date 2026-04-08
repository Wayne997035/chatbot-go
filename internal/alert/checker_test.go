package alert

import (
	"context"
	"testing"
)

func TestCheckerShouldNotifyNilRedis(t *testing.T) {
	c := &Checker{redisClient: nil}

	// When Redis is nil, shouldNotify should always return true (allow notification).
	got := c.shouldNotify(context.Background(), "any-key")
	if !got {
		t.Error("expected shouldNotify to return true when Redis is nil")
	}
}

func TestCheckerMarkNotifiedNilRedis(t *testing.T) {
	c := &Checker{redisClient: nil}

	// Should not panic when Redis is nil.
	c.markNotified(context.Background(), "any-key")
}
