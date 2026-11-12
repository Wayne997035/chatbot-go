package integration

import (
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/database/profile"
	userstore "chatbot-go/internal/storage/database/user"
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// [F30-78] 管理員統計的儲存層查詢：真 Mongo（testcontainers）；每個測試都用 >=2 組資料，只有 1 筆的資料抓不到分組 / 去重 / 分頁的錯.

func statsInsert(t *testing.T, db *mongo.Database, coll string, docs ...any) {
	t.Helper()
	if _, err := db.Collection(coll).InsertMany(context.Background(), docs); err != nil {
		t.Fatalf("insert into %s: %v", coll, err)
	}
}

func statsUID(n int) string { return fmt.Sprintf("U%032x", n) }

// 同 createTime 的 10 位排在中間，翻頁不得重複或漏.
func TestUserRepoListPageStable(t *testing.T) {
	db := getTestDB(t)
	repo := userstore.NewUserRepository(db)
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

	var docs []any
	var want []string
	for i := range 120 {
		ts := base.Add(-time.Duration(i) * time.Hour)
		if i >= 55 && i < 65 {
			ts = base.Add(-55 * time.Hour) // 10 位完全相同
		}
		id := statsUID(1000 - i) // ID 與插入順序相反，沒有 tiebreak 就會亂
		docs = append(docs, bson.M{"_id": id, "createTime": ts})
	}
	statsInsert(t, db, "user", docs...)

	type row struct {
		id string
		ts time.Time
	}
	var rows []row
	for i := range 120 {
		ts := base.Add(-time.Duration(i) * time.Hour)
		if i >= 55 && i < 65 {
			ts = base.Add(-55 * time.Hour)
		}
		rows = append(rows, row{statsUID(1000 - i), ts})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].ts.Equal(rows[j].ts) {
			return rows[i].ts.After(rows[j].ts)
		}
		return rows[i].id < rows[j].id
	})
	for _, r := range rows {
		want = append(want, r.id)
	}

	ctx := context.Background()
	var got []string
	seen := map[string]bool{}
	for _, p := range []struct{ offset, size int }{{0, 50}, {50, 50}, {100, 20}} {
		page, err := repo.ListUsersPage(ctx, p.offset, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != p.size {
			t.Fatalf("offset %d: %d users, want %d", p.offset, len(page), p.size)
		}
		for _, u := range page {
			if seen[u.ID] {
				t.Fatalf("duplicate %s across pages", u.ID)
			}
			seen[u.ID] = true
			got = append(got, u.ID)
		}
	}
	if len(got) != 120 {
		t.Fatalf("union = %d, want 120", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestStatsStoreJoinedBoundary(t *testing.T) {
	db := getTestDB(t)
	repo := userstore.NewUserRepository(db)
	since := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	statsInsert(t, db, "user",
		bson.M{"_id": statsUID(1), "createTime": since.Add(-time.Millisecond)}, // 差 1ms：不算
		bson.M{"_id": statsUID(2), "createTime": since},                        // 恰好：算（>=）
		bson.M{"_id": statsUID(3), "createTime": since.Add(time.Hour)},
		bson.M{"_id": statsUID(4), "createTime": since.Add(-30 * 24 * time.Hour)},
		bson.M{"_id": statsUID(5)}, // 缺 createTime：不計入近 N 天，但計入總數
	)
	ctx := context.Background()
	n, err := repo.CountUsersCreatedSince(ctx, since)
	if err != nil || n != 2 {
		t.Errorf("CountUsersCreatedSince = %d, %v; want 2", n, err)
	}
	total, err := repo.CountUsers(ctx)
	if err != nil || total != 5 {
		t.Errorf("CountUsers = %d, %v; want 5", total, err)
	}
}

func TestStatsStoreAnyEnabledDistinct(t *testing.T) {
	db := getTestDB(t)
	repo := alertsub.NewAlertSubRepository(db)
	now := time.Now()
	sub := func(user, typ string, enabled bool) bson.M {
		return bson.M{"_id": user + "_" + typ, "userId": user, "alertType": typ, "enabled": enabled, "regions": bson.A{}, "createTime": now, "updateTime": now}
	}
	statsInsert(t, db, "alertSubscription",
		sub("A", alertsub.TypeWeatherWarning, true), sub("A", alertsub.TypeEarthquakeTsunami, true), // A 兩類都開：distinct 只算 1
		sub("B", alertsub.TypeEarthquakeTsunami, true),
		sub("C", alertsub.TypeWeatherWarning, false), sub("C", alertsub.TypeEarthquakeTsunami, false), // C 全關：不算
	)
	ctx := context.Background()
	distinct, err := repo.CountDistinctEnabledUsers(ctx)
	if err != nil || distinct != 2 {
		t.Errorf("CountDistinctEnabledUsers = %d, %v; want 2", distinct, err)
	}
	for typ, want := range map[string]int64{alertsub.TypeWeatherWarning: 1, alertsub.TypeEarthquakeTsunami: 2} {
		if n, err := repo.CountEnabledByType(ctx, typ); err != nil || n != want {
			t.Errorf("CountEnabledByType(%s) = %d, %v; want %d", typ, n, err, want)
		}
	}
}

func TestStatsStoreProfilesFilledIgnoresShell(t *testing.T) {
	db := getTestDB(t)
	repo := profile.NewProfileRepository(db)
	now := time.Now()
	statsInsert(t, db, "profile",
		bson.M{"_id": "p1", "weightKg": 70.0, "updateTime": now},
		bson.M{"_id": "p2", "updateTime": now}, // 空殼
		bson.M{"_id": "p3", "sex": "female", "updateTime": now},
		bson.M{"_id": "p4", "heightCm": 170.0, "updateTime": now},
		bson.M{"_id": "p5", "age": 0, "sex": "", "updateTime": now}, // 欄位在但是空值
		bson.M{"_id": "p6", "age": 30, "updateTime": now},
	)
	n, err := repo.CountFilledProfiles(context.Background())
	if err != nil || n != 4 {
		t.Errorf("CountFilledProfiles = %d, %v; want 4 (p1, p3, p4, p6)", n, err)
	}
}

func TestStatsStoreMissingCreateTimeLast(t *testing.T) {
	db := getTestDB(t)
	repo := userstore.NewUserRepository(db)
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	statsInsert(t, db, "user",
		bson.M{"_id": statsUID(1)}, bson.M{"_id": statsUID(2)}, // 缺 createTime，ID 比有時間的小
		bson.M{"_id": statsUID(9), "createTime": base.Add(-time.Hour)},
		bson.M{"_id": statsUID(8), "createTime": base},
		bson.M{"_id": statsUID(7), "createTime": base.Add(-48 * time.Hour)},
	)
	page, err := repo.ListUsersPage(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{statsUID(8), statsUID(9), statsUID(7), statsUID(1), statsUID(2)}
	if len(page) != len(want) {
		t.Fatalf("got %d users, want %d", len(page), len(want))
	}
	for i, u := range page {
		if u.ID != want[i] {
			t.Errorf("position %d = %s, want %s", i, u.ID, want[i])
		}
	}
	if !page[3].CreateTime.IsZero() || !page[4].CreateTime.IsZero() {
		t.Errorf("missing createTime must decode to the zero time")
	}
}

func TestStatsStoreRegionGroups(t *testing.T) {
	db := getTestDB(t)
	repo := alertsub.NewAlertSubRepository(db)
	now := time.Now()
	sub := func(user, typ string, enabled bool, regions ...string) bson.M {
		return bson.M{"_id": user + "_" + typ, "userId": user, "alertType": typ, "enabled": enabled, "regions": regions, "createTime": now, "updateTime": now}
	}
	statsInsert(t, db, "alertSubscription",
		sub("A", alertsub.TypeWeatherWarning, true, "台北市", "台北市大安區"),
		sub("B", alertsub.TypeWeatherWarning, true, "台北市"),
		sub("C", alertsub.TypeWeatherWarning, false, "高雄市"),       // 關閉：不計
		sub("D", alertsub.TypeWeatherWarning, true, "台北市", "台北市"), // 同一筆內重複：只算 1 人
		sub("E", alertsub.TypeEarthquakeTsunami, true, "台中市"),     // 非天氣特報：不計
	)
	ctx := context.Background()
	rows, err := repo.CountWeatherWarningRegions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Region] = r.Count
	}
	want := map[string]int64{"台北市": 3, "台北市大安區": 1}
	if len(got) != len(want) || got["台北市"] != 3 || got["台北市大安區"] != 1 {
		t.Errorf("regions = %v, want %v", got, want)
	}

	subs, err := repo.FindSubscriptionsByUserIDs(ctx, []string{"A", "C"})
	if err != nil || len(subs) != 2 {
		t.Fatalf("FindSubscriptionsByUserIDs(A,C) = %d, %v; want 2", len(subs), err)
	}
	for _, s := range subs {
		if s.UserID != "A" && s.UserID != "C" {
			t.Errorf("unexpected user %s", s.UserID)
		}
	}
	if subs, err := repo.FindSubscriptionsByUserIDs(ctx, nil); err != nil || len(subs) != 0 {
		t.Errorf("empty id list = %d, %v; want none", len(subs), err)
	}
}
