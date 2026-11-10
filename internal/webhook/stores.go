package webhook

import (
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/database/profile"
	userstore "chatbot-go/internal/storage/database/user"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"time"
)

// webhook 用到的儲存操作（使用端定義的小介面）. 飲食紀錄幾乎每個操作都會用到，直接用 meal.MealRepository.
type (
	// UserStore 加好友時建立使用者、刪除資料時刪除.
	UserStore interface {
		Upsert(ctx context.Context, u *userstore.User) error
		Delete(ctx context.Context, id string) error
	}
	// SubscriptionStore 開關通知、封鎖或刪除資料時移除所有訂閱.
	SubscriptionStore interface {
		Upsert(ctx context.Context, sub *alertsub.AlertSubscription) error
		Delete(ctx context.Context, id string) error
		DeleteByUser(ctx context.Context, userID string) error
	}
	// DistrictLister 模糊比對地名用的行政區清單.
	DistrictLister interface {
		FindAllDistricts(ctx context.Context) ([]weatherstore.DistrictEntry, error)
	}
	// WorkoutStore 記錄、查詢與刪除運動.
	WorkoutStore interface {
		Insert(ctx context.Context, w *workout.Workout) error
		FindByUserBetween(ctx context.Context, userID string, from, to time.Time) ([]workout.Workout, error)
		DeleteLatest(ctx context.Context, userID string, since time.Time) (*workout.Workout, error)
		// DeleteByID [F291-04] 補刪這次寫的那一筆（不動使用者刪除之後新記的運動）.
		DeleteByID(ctx context.Context, userID, id string) (bool, error)
		DeleteAllByUser(ctx context.Context, userID string) error
	}
	// ProfileStore 讀取身體資料、只更新提到的欄位、刪除.
	ProfileStore interface {
		Get(ctx context.Context, userID string) (*profile.Profile, error)
		Merge(ctx context.Context, patch *profile.Profile) (*profile.Profile, error)
		Delete(ctx context.Context, userID string) error
	}
)
