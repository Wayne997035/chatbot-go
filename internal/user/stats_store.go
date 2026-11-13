package user

import (
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/database/profile"
	"context"
	"time"

	userstore "chatbot-go/internal/storage/database/user"
)

// UsageReader [F30-76] 氣象署用量的唯讀來源；只有一個方法，統計 API 不寫入、不重置、不觸發額度判斷.
type UsageReader interface {
	Usage(ctx context.Context) cwa.Usage
}

// StatsStore [F30-71] 統計用的唯讀儲存介面（user / alertsub / profile 三組唯讀查詢合在一起）.
type StatsStore interface {
	CountUsers(ctx context.Context) (int64, error)
	CountUsersCreatedSince(ctx context.Context, since time.Time) (int64, error)
	ListUsersPage(ctx context.Context, offset, limit int) ([]userstore.User, error) // createTime 降冪（缺值排最後）、_id 升冪
	CountEnabledByType(ctx context.Context, alertType string) (int64, error)
	CountDistinctEnabledUsers(ctx context.Context) (int64, error)
	CountWeatherWarningRegions(ctx context.Context) ([]alertsub.RegionCount, error) // enabled=true 的天氣特報，依 regions 原字串計人數
	FindSubscriptionsByUserIDs(ctx context.Context, userIDs []string) ([]alertsub.AlertSubscription, error)
	CountFilledProfiles(ctx context.Context) (int64, error)
}

// statsStore [F30-78] 把三個儲存庫的唯讀方法逐一轉呼叫；不內嵌，免得寫入方法跟著露出來.
type statsStore struct {
	users    *userstore.UserRepo
	subs     *alertsub.AlertSubRepo
	profiles *profile.ProfileRepo
}

// NewStatsStore 組合三個儲存庫成 StatsStore（只轉呼叫唯讀查詢）.
func NewStatsStore(users *userstore.UserRepo, subs *alertsub.AlertSubRepo, profiles *profile.ProfileRepo) StatsStore {
	return &statsStore{users: users, subs: subs, profiles: profiles}
}

func (s *statsStore) CountUsers(ctx context.Context) (int64, error) { return s.users.CountUsers(ctx) }

func (s *statsStore) CountUsersCreatedSince(ctx context.Context, since time.Time) (int64, error) {
	return s.users.CountUsersCreatedSince(ctx, since)
}

func (s *statsStore) ListUsersPage(ctx context.Context, offset, limit int) ([]userstore.User, error) {
	return s.users.ListUsersPage(ctx, offset, limit)
}

func (s *statsStore) CountEnabledByType(ctx context.Context, alertType string) (int64, error) {
	return s.subs.CountEnabledByType(ctx, alertType)
}

func (s *statsStore) CountDistinctEnabledUsers(ctx context.Context) (int64, error) {
	return s.subs.CountDistinctEnabledUsers(ctx)
}

func (s *statsStore) CountWeatherWarningRegions(ctx context.Context) ([]alertsub.RegionCount, error) {
	return s.subs.CountWeatherWarningRegions(ctx)
}

func (s *statsStore) FindSubscriptionsByUserIDs(ctx context.Context, userIDs []string) ([]alertsub.AlertSubscription, error) {
	return s.subs.FindSubscriptionsByUserIDs(ctx, userIDs)
}

func (s *statsStore) CountFilledProfiles(ctx context.Context) (int64, error) {
	return s.profiles.CountFilledProfiles(ctx)
}
