package recentmeal

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStoreIsPerUserAndExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := New(nil)
	s.Now = func() time.Time { return now }

	s.Remember(ctx, "U1", "m1", "text")
	if m, ok := s.Get(ctx, "U1"); !ok || m.ID != "m1" || m.Source != "text" || !m.At.Equal(now) {
		t.Fatalf("get = %+v %v", m, ok)
	}
	if _, ok := s.Get(ctx, "U2"); ok {
		t.Error("another user has no recent meal")
	}
	s.Now = func() time.Time { return now.Add(TTL + time.Second) }
	if _, ok := s.Get(ctx, "U1"); ok {
		t.Error("expired after the TTL")
	}
	s.Now = func() time.Time { return now }
	if err := s.Forget(ctx, "U1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(ctx, "U1"); ok {
		t.Error("forgotten")
	}
}

func TestMemoryStorePrunesExpiredEntries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := New(nil)
	s.Now = func() time.Time { return now }
	for i := range maxLocal {
		s.Remember(ctx, "old"+string(rune('a'+i%26))+time.Duration(i).String(), "m", "text")
	}
	s.Now = func() time.Time { return now.Add(TTL + time.Minute) }
	s.Remember(ctx, "U1", "m1", "text")
	if len(s.local) != 1 {
		t.Errorf("expired entries must be pruned, %d left", len(s.local))
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	s.Remember(context.Background(), "U1", "m1", "text")
	if _, ok := s.Get(context.Background(), "U1"); ok {
		t.Error("nil store remembers nothing")
	}
	if err := s.Forget(context.Background(), "U1"); err != nil {
		t.Error(err)
	}
}
