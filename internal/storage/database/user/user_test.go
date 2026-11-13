package userstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// F30-03 User 的名稱 / 頭像欄位：只進資料庫、不進任何 JSON. [F31-01] User 不再有 email 欄.

func sampleUser() User {
	return User{
		ID: "U1", CreateTime: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		DisplayName: "SECRET-NAME", PictureURL: "https://example.test/secret.png",
	}
}

// A5：json.Marshal 不含名稱 / 頭像鍵與值（[F31-01] email 鍵也不得出現）.
func TestUserJSONNeverExposesProfileFields(t *testing.T) {
	b, err := json.Marshal(sampleUser())
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, banned := range []string{"displayName", "pictureUrl", "email", "SECRET-NAME", "secret.png"} {
		if strings.Contains(out, banned) {
			t.Errorf("json output %s exposes %q", out, banned)
		}
	}
	if !strings.Contains(out, `"id":"U1"`) {
		t.Errorf("json output lost existing fields: %s", out)
	}
}

// A5 反例的另一半：bson 仍含名稱 / 頭像（DB 有存）、不含 email [F31-01]，且缺欄位的舊文件讀出為空字串.
func TestUserBSONKeepsProfileFields(t *testing.T) {
	raw, err := bson.Marshal(sampleUser())
	if err != nil {
		t.Fatal(err)
	}
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"displayName": "SECRET-NAME", "pictureUrl": "https://example.test/secret.png"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("bson %s = %v, want %q", k, m[k], v)
		}
	}
	if _, ok := m["email"]; ok {
		t.Errorf("[F31-01] bson must not carry an email key: %v", m)
	}

	old, err := bson.Marshal(bson.M{"_id": "U9", "createTime": time.Now(), "type": "user"})
	if err != nil {
		t.Fatal(err)
	}
	var u User
	if err := bson.Unmarshal(old, &u); err != nil {
		t.Fatal(err)
	}
	if u.DisplayName != "" || u.PictureURL != "" {
		t.Errorf("legacy document must read empty profile fields: %+v", u)
	}
}

// [F31-01] 還沒被同步清掉 email 欄的既有文件仍可讀出（未知欄位不報錯、也不進 User）.
func TestUserDecodesLegacyDocumentWithEmailKey(t *testing.T) {
	raw, err := bson.Marshal(bson.M{"_id": "U8", "createTime": time.Now(), "email": "old@x.test", "displayName": "N"})
	if err != nil {
		t.Fatal(err)
	}
	var u User
	if err := bson.Unmarshal(raw, &u); err != nil {
		t.Fatalf("legacy document with email key must decode: %v", err)
	}
	if u.ID != "U8" || u.DisplayName != "N" {
		t.Errorf("decoded %+v", u)
	}
}

// [F31-01] SaveLineProfile 的更新內容：名稱 / 頭像 $set（空字串也寫）、email 一律 $unset（清掉既有欄位）、
// createTime 只在新建時寫、不設 type；$set 與 $setOnInsert 都不得出現 email.
func TestLineProfileUpdateShape(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		n, p     string
		wantName string
	}{
		{"all set", "Taro", "https://x.test/a.png", "Taro"},
		{"claims absent clears old values", "", "", ""},
		{"name only", "Hanako", "", "Hanako"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upd := lineProfileUpdate(tc.n, tc.p, now)
			set, _ := upd["$set"].(bson.M)
			if set["displayName"] != tc.wantName || set["pictureUrl"] != tc.p {
				t.Errorf("$set = %v", set)
			}
			if len(set) != 2 {
				t.Errorf("$set must carry exactly displayName and pictureUrl (empty strings included): %v", set)
			}
			if _, ok := set["email"]; ok {
				t.Errorf("$set must not carry email: %v", set)
			}
			unset, _ := upd["$unset"].(bson.M)
			if v, ok := unset["email"]; !ok || v != "" || len(unset) != 1 {
				t.Errorf("$unset = %v, want exactly {email: \"\"}", unset)
			}
			ins, _ := upd["$setOnInsert"].(bson.M)
			if ins["createTime"] != now || len(ins) != 1 {
				t.Errorf("$setOnInsert = %v, want only createTime (no type, no email)", ins)
			}
			if len(upd) != 3 {
				t.Errorf("update must have exactly $set / $unset / $setOnInsert: %v", upd)
			}
		})
	}
}

// F30-08：Upsert 失敗的 log 不含 userID、email、主機:埠.
func TestUpsertFailureLogIsRedacted(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	uid := "U" + strings.Repeat("a1", 16)
	logUpsertError(errors.New("write " + uid + " a@b.test to 10.0.0.5:27017 failed"))

	out := buf.String()
	for _, leak := range []string{uid, "a@b.test", "10.0.0.5:27017"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "user upsert failed") {
		t.Errorf("log line missing: %s", out)
	}
}
