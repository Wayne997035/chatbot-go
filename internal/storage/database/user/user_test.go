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

// F30-03 User 的三個個資欄位：只進資料庫、不進任何 JSON.

func sampleUser() User {
	return User{
		ID: "U1", CreateTime: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		DisplayName: "SECRET-NAME", PictureURL: "https://example.test/secret.png", Email: "secret@x.test",
	}
}

// A5：json.Marshal 不含三個鍵，也不含三個值.
func TestUserJSONNeverExposesProfileFields(t *testing.T) {
	b, err := json.Marshal(sampleUser())
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, banned := range []string{"displayName", "pictureUrl", "email", "SECRET-NAME", "secret.png", "secret@x.test"} {
		if strings.Contains(out, banned) {
			t.Errorf("json output %s exposes %q", out, banned)
		}
	}
	if !strings.Contains(out, `"id":"U1"`) {
		t.Errorf("json output lost existing fields: %s", out)
	}
}

// A5 反例的另一半：bson 仍含三欄（DB 有存），且缺欄位的舊文件讀出為空字串.
func TestUserBSONKeepsProfileFields(t *testing.T) {
	raw, err := bson.Marshal(sampleUser())
	if err != nil {
		t.Fatal(err)
	}
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"displayName": "SECRET-NAME", "pictureUrl": "https://example.test/secret.png", "email": "secret@x.test"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("bson %s = %v, want %q", k, m[k], v)
		}
	}

	old, err := bson.Marshal(bson.M{"_id": "U9", "createTime": time.Now(), "type": "user"})
	if err != nil {
		t.Fatal(err)
	}
	var u User
	if err := bson.Unmarshal(old, &u); err != nil {
		t.Fatal(err)
	}
	if u.DisplayName != "" || u.PictureURL != "" || u.Email != "" {
		t.Errorf("legacy document must read empty profile fields: %+v", u)
	}
}

// SaveLineProfile 的更新內容：三欄 $set（空字串也寫）、createTime 只在新建時寫、不設 type.
func TestLineProfileUpdateShape(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name                string
		n, p, e             string
		wantName, wantEmail string
	}{
		{"all set", "Taro", "https://x.test/a.png", "a@b.co", "Taro", "a@b.co"},
		{"claims absent clears old values", "", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upd := lineProfileUpdate(tc.n, tc.p, tc.e, now)
			set, _ := upd["$set"].(bson.M)
			if set["displayName"] != tc.wantName || set["email"] != tc.wantEmail || set["pictureUrl"] != tc.p {
				t.Errorf("$set = %v", set)
			}
			if len(set) != 3 {
				t.Errorf("$set must carry exactly the three profile fields (empty strings included): %v", set)
			}
			ins, _ := upd["$setOnInsert"].(bson.M)
			if ins["createTime"] != now || len(ins) != 1 {
				t.Errorf("$setOnInsert = %v, want only createTime (no type)", ins)
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
