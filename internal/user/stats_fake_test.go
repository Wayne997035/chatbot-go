package user

import (
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	userstore "chatbot-go/internal/storage/database/user"
)

var fixedNow = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

// 假 store 的 error 內含完整 ID 與連線字串，用來證明回應與 log 都不外洩.
const (
	fakeErrSecret = "SECRET-STORE-FAILURE"
	fakeErrID     = "U0123456789abcdef0123456789abcdef"
)

// uid 產生 U + 32 位小寫 16 進位的 LINE userID 形狀字串.
func uid(n int) string { return fmt.Sprintf("U%032x", n) }

type fakeProfile struct {
	weight, height float64
	age            int
	sex            string
}

// fakeStore 依 D4 的分工實作：CreateTime >= since、createTime 降冪（缺值最後）+ _id 升冪；地區故意回亂序.
type fakeStore struct {
	mu         sync.Mutex
	users      []userstore.User
	subs       []alertsub.AlertSubscription
	profiles   []fakeProfile
	regionsOut []alertsub.RegionCount // 非 nil 時直接當 region 分組結果
	failAt     string
	hook       func(ctx context.Context, method string) error
	calls      []string
	inflight   int
	overlapped bool
	sinceArgs  []time.Time
	offsets    []int
}

func (f *fakeStore) enter(ctx context.Context, method string) (leave func(), err error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.inflight++
	if f.inflight > 1 {
		f.overlapped = true
	}
	hook, failAt := f.hook, f.failAt
	f.mu.Unlock()
	leave = func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}
	time.Sleep(time.Millisecond) // 拉長呼叫區間，並行呼叫才看得出重疊
	if hook != nil {
		if herr := hook(ctx, method); herr != nil {
			return leave, herr
		}
	}
	if failAt == method {
		return leave, errors.New(fakeErrSecret + " " + fakeErrID)
	}
	return leave, nil
}

func (f *fakeStore) CountUsers(ctx context.Context) (int64, error) {
	leave, err := f.enter(ctx, "CountUsers")
	defer leave()
	return int64(len(f.users)), err
}

func (f *fakeStore) CountUsersCreatedSince(ctx context.Context, since time.Time) (int64, error) {
	leave, err := f.enter(ctx, "CountUsersCreatedSince")
	defer leave()
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	f.sinceArgs = append(f.sinceArgs, since)
	f.mu.Unlock()
	var n int64
	for _, u := range f.users {
		if !u.CreateTime.IsZero() && !u.CreateTime.Before(since) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) ListUsersPage(ctx context.Context, offset, limit int) ([]userstore.User, error) {
	leave, err := f.enter(ctx, "ListUsersPage")
	defer leave()
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.offsets = append(f.offsets, offset)
	f.mu.Unlock()
	sorted := append([]userstore.User(nil), f.users...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.CreateTime.IsZero() != b.CreateTime.IsZero() {
			return !a.CreateTime.IsZero()
		}
		if !a.CreateTime.Equal(b.CreateTime) {
			return a.CreateTime.After(b.CreateTime)
		}
		return a.ID < b.ID
	})
	if offset >= len(sorted) {
		return nil, nil
	}
	end := min(offset+limit, len(sorted))
	return sorted[offset:end], nil
}

func (f *fakeStore) CountEnabledByType(ctx context.Context, alertType string) (int64, error) {
	leave, err := f.enter(ctx, "CountEnabledByType")
	defer leave()
	var n int64
	for _, s := range f.subs {
		if s.Enabled && s.AlertType == alertType {
			n++
		}
	}
	return n, err
}

func (f *fakeStore) CountDistinctEnabledUsers(ctx context.Context) (int64, error) {
	leave, err := f.enter(ctx, "CountDistinctEnabledUsers")
	defer leave()
	seen := map[string]bool{}
	for _, s := range f.subs {
		if s.Enabled {
			seen[s.UserID] = true
		}
	}
	return int64(len(seen)), err
}

func (f *fakeStore) CountWeatherWarningRegions(ctx context.Context) ([]alertsub.RegionCount, error) {
	leave, err := f.enter(ctx, "CountWeatherWarningRegions")
	defer leave()
	if err != nil {
		return nil, err
	}
	if f.regionsOut != nil {
		return f.regionsOut, nil
	}
	counts := map[string]int64{}
	for _, s := range f.subs {
		if s.Enabled && s.AlertType == alertsub.TypeWeatherWarning {
			for _, r := range s.Regions {
				counts[r]++
			}
		}
	}
	var out []alertsub.RegionCount
	for r, c := range counts { // map 迭代順序隨機：handler 必須自己排序
		out = append(out, alertsub.RegionCount{Region: r, Count: c})
	}
	return out, nil
}

func (f *fakeStore) FindSubscriptionsByUserIDs(ctx context.Context, userIDs []string) ([]alertsub.AlertSubscription, error) {
	leave, err := f.enter(ctx, "FindSubscriptionsByUserIDs")
	defer leave()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range userIDs {
		want[id] = true
	}
	var out []alertsub.AlertSubscription
	for _, s := range f.subs {
		if want[s.UserID] {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) CountFilledProfiles(ctx context.Context) (int64, error) {
	leave, err := f.enter(ctx, "CountFilledProfiles")
	defer leave()
	var n int64
	for _, p := range f.profiles {
		if p.weight > 0 || p.height > 0 || p.age > 0 || p.sex != "" {
			n++
		}
	}
	return n, err
}

// fakeUsage 只實作 Usage（規格要求：介面只有一個方法）.
type fakeUsage struct{ u cwa.Usage }

func (f fakeUsage) Usage(context.Context) cwa.Usage { return f.u }

func weatherSub(userID string, enabled bool, regions ...string) alertsub.AlertSubscription {
	return alertsub.AlertSubscription{
		ID: userID + "_" + alertsub.TypeWeatherWarning, UserID: userID, AlertType: alertsub.TypeWeatherWarning,
		Regions: regions, Enabled: enabled,
	}
}

func quakeSub(userID string, enabled bool) alertsub.AlertSubscription {
	return alertsub.AlertSubscription{
		ID: userID + "_" + alertsub.TypeEarthquakeTsunami, UserID: userID, AlertType: alertsub.TypeEarthquakeTsunami, Enabled: enabled,
	}
}
