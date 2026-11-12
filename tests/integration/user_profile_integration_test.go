package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	userstore "chatbot-go/internal/storage/database/user"
)

// profileUserID 產生測試用假 LINE userID（U + 32 位小寫 hex），每支測試用不同的 n.
func profileUserID(n int) string {
	return fmt.Sprintf("U%032x", n)
}

// readRawUser 直接讀 user collection 的原始文件，用來分辨「鍵存在但為空字串」與「鍵不存在」.
func readRawUser(ctx context.Context, t *testing.T, db *mongo.Database, id string) bson.M {
	t.Helper()
	var raw bson.M
	if err := db.Collection("user").FindOne(ctx, bson.M{"_id": id}).Decode(&raw); err != nil {
		t.Fatalf("raw find %s: %v", id, err)
	}
	return raw
}

func assertProfile(ctx context.Context, t *testing.T, repo *userstore.UserRepo, id, name, pic, email string) *userstore.User {
	t.Helper()
	u, err := repo.FindByID(ctx, id)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if u == nil {
		t.Fatalf("FindByID returned nil for existing user")
	}
	if u.DisplayName != name || u.PictureURL != pic || u.Email != email {
		t.Errorf("profile = (%q,%q,%q), want (%q,%q,%q)", u.DisplayName, u.PictureURL, u.Email, name, pic, email)
	}
	return u
}

func assertNoTypeKey(t *testing.T, raw bson.M) {
	t.Helper()
	if v, ok := raw["type"]; ok {
		t.Errorf("raw document has type key = %v, want absent", v)
	}
}

// assertCreateTimeBetween 兩端與讀回值都截到毫秒（Mongo 只存毫秒）後比較.
func assertCreateTimeBetween(t *testing.T, got, before, after time.Time) {
	t.Helper()
	got, before, after = got.Truncate(time.Millisecond), before.Truncate(time.Millisecond), after.Truncate(time.Millisecond)
	if got.Before(before) || got.After(after) {
		t.Errorf("createTime = %v, want within [%v, %v]", got, before, after)
	}
}

// [F30-28] 新建（含 Delete 後重建）：不設 type，createTime 由 $setOnInsert 寫入.
func TestSaveLineProfileCreatesWithoutType(t *testing.T) {
	db := getTestDB(t)
	repo := userstore.NewUserRepository(db)
	ctx := context.Background()

	t.Run("new user never written before", func(t *testing.T) {
		id := profileUserID(1)
		before := time.Now()
		if err := repo.SaveLineProfile(ctx, id, "測試名稱", "https://example.test/p.png", "a@example.test"); err != nil {
			t.Fatalf("SaveLineProfile: %v", err)
		}
		after := time.Now()

		u := assertProfile(ctx, t, repo, id, "測試名稱", "https://example.test/p.png", "a@example.test")
		assertCreateTimeBetween(t, u.CreateTime, before, after)
		assertNoTypeKey(t, readRawUser(ctx, t, db, id))
	})

	t.Run("recreated after delete", func(t *testing.T) {
		id := profileUserID(2)
		t0 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := repo.Upsert(ctx, &userstore.User{ID: id, CreateTime: t0, Type: "user"}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := repo.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		before := time.Now()
		if err := repo.SaveLineProfile(ctx, id, "重建", "https://example.test/q.png", "b@example.test"); err != nil {
			t.Fatalf("SaveLineProfile: %v", err)
		}
		after := time.Now()

		u := assertProfile(ctx, t, repo, id, "重建", "https://example.test/q.png", "b@example.test")
		assertCreateTimeBetween(t, u.CreateTime, before, after)
		if u.CreateTime.Equal(t0) {
			t.Errorf("createTime = t0 %v, want a fresh time after recreate", t0)
		}
		assertNoTypeKey(t, readRawUser(ctx, t, db, id))
	})
}

// [F30-28] 既有文件（聊天端先建）：SaveLineProfile 寫三欄，但保留 createTime 與 type.
func TestSaveLineProfileKeepsCreateTimeAndType(t *testing.T) {
	db := getTestDB(t)
	repo := userstore.NewUserRepository(db)
	ctx := context.Background()
	id := profileUserID(3)
	t0 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := repo.Upsert(ctx, &userstore.User{ID: id, CreateTime: t0, Type: "user"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.SaveLineProfile(ctx, id, "名稱", "https://example.test/p.png", "c@example.test"); err != nil {
		t.Fatalf("SaveLineProfile: %v", err)
	}

	u := assertProfile(ctx, t, repo, id, "名稱", "https://example.test/p.png", "c@example.test")
	if !u.CreateTime.Equal(t0) {
		t.Errorf("createTime = %v, want %v", u.CreateTime, t0)
	}
	if u.Type != "user" {
		t.Errorf("type = %q, want user", u.Type)
	}
	if raw := readRawUser(ctx, t, db, id); raw["type"] != "user" {
		t.Errorf("raw type = %v, want user", raw["type"])
	}
}

// [F30-28] claim 缺席：三欄以空字串明確寫入（鍵存在），舊值不殘留.
func TestSaveLineProfileClearsAbsentClaims(t *testing.T) {
	db := getTestDB(t)
	repo := userstore.NewUserRepository(db)
	ctx := context.Background()
	id := profileUserID(4)

	if err := repo.SaveLineProfile(ctx, id, "名稱", "https://example.test/p.png", "d@example.test"); err != nil {
		t.Fatalf("first SaveLineProfile: %v", err)
	}
	if err := repo.SaveLineProfile(ctx, id, "", "", ""); err != nil {
		t.Fatalf("second SaveLineProfile: %v", err)
	}

	assertProfile(ctx, t, repo, id, "", "", "")
	raw := readRawUser(ctx, t, db, id)
	for _, k := range []string{"displayName", "pictureUrl", "email"} {
		v, ok := raw[k]
		if !ok {
			t.Errorf("raw key %s absent, want present as empty string", k)
			continue
		}
		if v != "" {
			t.Errorf("raw key %s = %v, want empty string", k, v)
		}
	}
}

// [F30-28] 先 LIFF 後聊天：Upsert 是 $setOnInsert，既有文件不被更新（三欄、createTime 不變，type 仍不存在）.
func TestUpsertAfterSaveLineProfileKeepsProfile(t *testing.T) {
	db := getTestDB(t)
	repo := userstore.NewUserRepository(db)
	ctx := context.Background()
	id := profileUserID(5)

	before := time.Now()
	if err := repo.SaveLineProfile(ctx, id, "名稱", "https://example.test/p.png", "e@example.test"); err != nil {
		t.Fatalf("SaveLineProfile: %v", err)
	}
	after := time.Now()
	first := assertProfile(ctx, t, repo, id, "名稱", "https://example.test/p.png", "e@example.test")
	assertCreateTimeBetween(t, first.CreateTime, before, after)

	other := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := repo.Upsert(ctx, &userstore.User{ID: id, CreateTime: other, Type: "user"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	u := assertProfile(ctx, t, repo, id, "名稱", "https://example.test/p.png", "e@example.test")
	if !u.CreateTime.Equal(first.CreateTime) {
		t.Errorf("createTime = %v, want unchanged %v", u.CreateTime, first.CreateTime)
	}
	assertNoTypeKey(t, readRawUser(ctx, t, db, id))
}
