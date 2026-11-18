package storagetest

import (
	"chatbot-go/internal/assistant/recordedit"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// PendingEdits [F33-21] 記憶體版的 recordedit.Store：每個 userID 一筆、Take 原子取出並刪除、nonce 以 subtle.ConstantTimeCompare 比對.
// 以 userID 為鍵（不是忽略 userID 的 stub），跨使用者測試才有意義.
type PendingEdits struct {
	mu    sync.Mutex
	items map[string]recordedit.Pending

	// PutErr / TakeErr / DeleteErr 非 nil 時對應方法直接回這個錯誤（模擬暫存後端故障）.
	PutErr, TakeErr, DeleteErr, PeekErr error
	// 呼叫次數（測試用）.
	Puts, Takes, Deletes, Peeks atomic.Int64
}

var _ recordedit.Store = (*PendingEdits)(nil)

// NewPendingEdits 建立空的暫存.
func NewPendingEdits() *PendingEdits { return &PendingEdits{items: map[string]recordedit.Pending{}} }

// Len 目前暫存的筆數.
func (s *PendingEdits) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// Peek 看某人目前的待確認項目（不取出）；沒有回 nil.
func (s *PendingEdits) Peek(userID string) *recordedit.Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[userID]
	if !ok {
		return nil
	}
	return &p
}

// PeekPending [F34-16] 不消耗地讀出未過期的項目；沒有或過期回 ErrNotFound（過期不刪，與 Redis/Mongo 一樣只是讀不到）.
func (s *PendingEdits) PeekPending(_ context.Context, userID string, now time.Time) (*recordedit.Pending, error) {
	s.Peeks.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PeekErr != nil {
		return nil, s.PeekErr
	}
	p, ok := s.items[userID]
	if !ok || !now.Before(p.ExpiresAt) {
		return nil, recordedit.ErrNotFound
	}
	return &p, nil
}

// Put 覆蓋該使用者既有的項目.
func (s *PendingEdits) Put(_ context.Context, userID string, p *recordedit.Pending) error {
	s.Puts.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PutErr != nil {
		return s.PutErr
	}
	if userID == "" || p == nil || !recordedit.ValidNonce(p.Nonce) {
		return errors.New("pending edit: invalid put")
	}
	s.items[userID] = *p
	return nil
}

// Take 比對 nonce、檢查未過期、取出並刪除；nonce 不符不動原項目.
func (s *PendingEdits) Take(_ context.Context, userID, nonce string, now time.Time) (*recordedit.Pending, error) {
	s.Takes.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.TakeErr != nil {
		return nil, s.TakeErr
	}
	p, ok := s.items[userID]
	if !ok || subtle.ConstantTimeCompare([]byte(p.Nonce), []byte(nonce)) != 1 {
		return nil, recordedit.ErrNotFound
	}
	delete(s.items, userID)
	if !now.Before(p.ExpiresAt) {
		return nil, recordedit.ErrNotFound
	}
	return &p, nil
}

// Delete 刪除該使用者的項目；不存在不算錯.
func (s *PendingEdits) Delete(_ context.Context, userID string) error {
	s.Deletes.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DeleteErr != nil {
		return s.DeleteErr
	}
	delete(s.items, userID)
	return nil
}

// pendingBase 契約測試的基準時間（整秒、UTC；MongoDB 只存到毫秒）.
// 刻意放在遙遠的未來：MongoDB 的 TTL 背景執行緒不會在 Put 與 Take 之間把測試資料清掉（過期的判斷只靠 Take 傳入的 now）.
var pendingBase = time.Date(2099, 1, 1, 10, 0, 0, 0, time.UTC)

func samplePending(t *testing.T) *recordedit.Pending {
	t.Helper()
	nonce, err := recordedit.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	return &recordedit.Pending{
		Nonce: nonce, Kind: recordedit.OutcomeProposal, ExpiresAt: pendingBase.Add(recordedit.PendingTTL),
		Proposal: &recordedit.Proposal{
			Kind: recordedit.KindWorkout, Op: recordedit.OpSetWorkout, RecordID: "w1", BaseVersion: 2,
			Workout: &recordedit.WorkoutResult{DurationMin: 30, DistanceKM: 6, Calories: 471, DoneAt: pendingBase.Add(-3 * time.Hour)},
			Summary: "要把「跑步」改成 30 分鐘嗎？", Before: 314, After: 471,
		},
	}
}

func marshalPending(t *testing.T, p *recordedit.Pending) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// PendingStoreContract [F33-21] recordedit.Store 的行為規格（A40）：記憶體、Redis（miniredis）、MongoDB 三個實作各跑同一組.
// 每個子測試都拿一個全新的儲存.
func PendingStoreContract(t *testing.T, newStore func(t *testing.T) recordedit.Store) {
	ctx := context.Background()
	now := pendingBase

	t.Run("put then take returns the same content, and only once", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		if err := s.Put(ctx, "U1", p); err != nil {
			t.Fatal(err)
		}
		got, err := s.Take(ctx, "U1", p.Nonce, now)
		if err != nil || got == nil || marshalPending(t, got) != marshalPending(t, p) {
			t.Fatalf("take: %+v err=%v want %s", got, err, marshalPending(t, p))
		}
		if again, err := s.Take(ctx, "U1", p.Nonce, now); again != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Fatalf("second take must be not-found: %+v %v", again, err)
		}
	})

	t.Run("a candidate pending round-trips", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		p.Kind, p.Proposal = recordedit.OutcomeClarify, nil
		p.Plan = &recordedit.Plan{Op: recordedit.OpSetItemKcal, Target: "M1", Item: 1, Kcal: 150}
		p.RecordKind = recordedit.KindMeal
		p.Candidates = []recordedit.Candidate{{RecordID: "m1", Label: "11/17 午餐 500大卡"}, {RecordID: "m2", Label: "11/16 午餐 650大卡"}}
		if err := s.Put(ctx, "U1", p); err != nil {
			t.Fatal(err)
		}
		got, err := s.Take(ctx, "U1", p.Nonce, now)
		if err != nil || marshalPending(t, got) != marshalPending(t, p) {
			t.Fatalf("take: %+v err=%v", got, err)
		}
	})

	t.Run("a wrong nonce is not-found and does not consume the valid pending", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		_ = s.Put(ctx, "U1", p)
		for _, bad := range []string{"", "x", p.Nonce + "x", "AAAAAAAAAAAAAAAAAAAAAA"} {
			if got, err := s.Take(ctx, "U1", bad, now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
				t.Fatalf("nonce %q: %+v %v", bad, got, err)
			}
		}
		if got, err := s.Take(ctx, "U1", p.Nonce, now); err != nil || got == nil {
			t.Fatalf("valid nonce after wrong ones: %+v %v", got, err)
		}
	})

	t.Run("expiry is decided by now against ExpiresAt", func(t *testing.T) {
		tests := []struct {
			name    string
			takeAt  func(p *recordedit.Pending) time.Time
			wantHit bool
		}{
			{"one second after expiry", func(p *recordedit.Pending) time.Time { return p.ExpiresAt.Add(time.Second) }, false},
			{"exactly at expiry counts as expired", func(p *recordedit.Pending) time.Time { return p.ExpiresAt }, false},
			{"one second before expiry", func(p *recordedit.Pending) time.Time { return p.ExpiresAt.Add(-time.Second) }, true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s := newStore(t)
				p := samplePending(t)
				if err := s.Put(ctx, "U1", p); err != nil {
					t.Fatal(err)
				}
				got, err := s.Take(ctx, "U1", p.Nonce, tt.takeAt(p))
				if tt.wantHit && (err != nil || got == nil) {
					t.Fatalf("want a hit: %+v %v", got, err)
				}
				if !tt.wantHit && (got != nil || !errors.Is(err, recordedit.ErrNotFound)) {
					t.Fatalf("want not-found: %+v %v", got, err)
				}
			})
		}
	})

	t.Run("another user cannot take it and the owner still can", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		_ = s.Put(ctx, "U1", p)
		if got, err := s.Take(ctx, "U2", p.Nonce, now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Fatalf("U2 took U1's pending: %+v %v", got, err)
		}
		if got, err := s.Take(ctx, "U1", p.Nonce, now); err != nil || got == nil {
			t.Fatalf("owner's pending is gone: %+v %v", got, err)
		}
	})

	t.Run("put twice keeps only the second", func(t *testing.T) {
		s := newStore(t)
		first, second := samplePending(t), samplePending(t)
		_ = s.Put(ctx, "U1", first)
		_ = s.Put(ctx, "U1", second)
		if got, err := s.Take(ctx, "U1", first.Nonce, now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Fatalf("first nonce must be invalid: %+v %v", got, err)
		}
		if got, err := s.Take(ctx, "U1", second.Nonce, now); err != nil || got == nil {
			t.Fatalf("second nonce: %+v %v", got, err)
		}
	})

	t.Run("delete removes only that user and a missing one is not an error", func(t *testing.T) {
		s := newStore(t)
		a, b := samplePending(t), samplePending(t)
		_ = s.Put(ctx, "U1", a)
		_ = s.Put(ctx, "U2", b)
		if err := s.Delete(ctx, "U1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "U-nobody"); err != nil {
			t.Fatalf("deleting a missing pending: %v", err)
		}
		if got, err := s.Take(ctx, "U1", a.Nonce, now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Fatalf("deleted pending was taken: %+v %v", got, err)
		}
		if got, err := s.Take(ctx, "U2", b.Nonce, now); err != nil || got == nil {
			t.Fatalf("other user's pending was deleted: %+v %v", got, err)
		}
	})

	t.Run("invalid input is rejected", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		bad := *p
		bad.Nonce = "short"
		for name, err := range map[string]error{
			"empty user":  s.Put(ctx, "", p),
			"nil pending": s.Put(ctx, "U1", nil),
			"bad nonce":   s.Put(ctx, "U1", &bad),
		} {
			if err == nil {
				t.Errorf("%s: want error", name)
			}
		}
		if got, err := s.Take(ctx, "", p.Nonce, now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Errorf("take with empty user: %+v %v", got, err)
		}
	})

	// [F34-16] PeekPending 四情境（規格 #28）：不存在 / 過期 / 他人 / 有效，且 PeekPending 不消耗.
	t.Run("PeekPending of a missing pending is not-found", func(t *testing.T) {
		s := newStore(t)
		if got, err := s.PeekPending(ctx, "U1", now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Fatalf("missing: %+v %v", got, err)
		}
		if got, err := s.PeekPending(ctx, "", now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Fatalf("empty user: %+v %v", got, err)
		}
	})

	t.Run("PeekPending of an expired pending is not-found", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		_ = s.Put(ctx, "U1", p)
		for name, at := range map[string]time.Time{"exactly at expiry": p.ExpiresAt, "after expiry": p.ExpiresAt.Add(time.Second)} {
			if got, err := s.PeekPending(ctx, "U1", at); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
				t.Fatalf("%s: %+v %v", name, got, err)
			}
		}
		if got, err := s.PeekPending(ctx, "U1", p.ExpiresAt.Add(-time.Second)); err != nil || got == nil {
			t.Fatalf("one second before expiry: %+v %v", got, err)
		}
	})

	t.Run("PeekPending never returns another user's pending", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		_ = s.Put(ctx, "U1", p)
		if got, err := s.PeekPending(ctx, "U2", now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Fatalf("U2 peeked U1's pending: %+v %v", got, err)
		}
		if got, err := s.PeekPending(ctx, "U1", now); err != nil || got == nil || got.Nonce != p.Nonce {
			t.Fatalf("owner peek: %+v %v", got, err)
		}
	})

	t.Run("Take still succeeds after PeekPending", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		_ = s.Put(ctx, "U1", p)
		for range 3 {
			got, err := s.PeekPending(ctx, "U1", now)
			if err != nil || got == nil || marshalPending(t, got) != marshalPending(t, p) {
				t.Fatalf("peek: %+v %v", got, err)
			}
		}
		got, err := s.Take(ctx, "U1", p.Nonce, now)
		if err != nil || got == nil {
			t.Fatalf("take after peek: %+v %v", got, err)
		}
		if again, err := s.PeekPending(ctx, "U1", now); again != nil || !errors.Is(err, recordedit.ErrNotFound) {
			t.Fatalf("peek after take must be not-found: %+v %v", again, err)
		}
	})

	// [F34-16] 舊暫存（Proposal 沒有 regroup 欄位）仍可解碼（規格 Data contracts：omitempty）.
	t.Run("a pending stored before regroup existed still decodes", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		if p.Proposal.Regroup != nil {
			t.Fatal("sample must not carry a regroup spec")
		}
		_ = s.Put(ctx, "U1", p)
		got, err := s.PeekPending(ctx, "U1", now)
		if err != nil || got == nil || got.Proposal == nil || got.Proposal.Regroup != nil {
			t.Fatalf("legacy pending: %+v %v", got, err)
		}
	})

	t.Run("twenty goroutines taking the same nonce succeed exactly once", func(t *testing.T) {
		s := newStore(t)
		p := samplePending(t)
		_ = s.Put(ctx, "U1", p)
		var wins, notFound, other atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, err := s.Take(ctx, "U1", p.Nonce, now)
				switch {
				case err == nil && got != nil:
					wins.Add(1)
				case errors.Is(err, recordedit.ErrNotFound):
					notFound.Add(1)
				default:
					other.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins.Load() != 1 || notFound.Load() != 19 || other.Load() != 0 {
			t.Fatalf("wins=%d notFound=%d other=%d, want 1/19/0", wins.Load(), notFound.Load(), other.Load())
		}
	})
}
