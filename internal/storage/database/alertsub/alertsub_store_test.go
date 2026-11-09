package alertsub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// [F24-99] 沒有要比對的地區就不查資料庫：連不上的 Mongo 也要立刻回空結果，而不是等逾時回錯誤.
func TestFindEnabledByTypeAndRegionsEmptySkipsQuery(t *testing.T) {
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100&connectTimeoutMS=100"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	repo := NewAlertSubRepository(client.Database("test"))

	start := time.Now()
	subs, err := repo.FindEnabledByTypeAndRegions(context.Background(), TypeWeatherWarning, nil)
	if elapsed := time.Since(start); err != nil || subs != nil || elapsed >= 100*time.Millisecond {
		t.Errorf("empty regions: subs=%v err=%v after %v, want nil, nil at once", subs, err, elapsed)
	}
	// 正對照：有地區時真的會去查（連不上就回錯誤），證明上面的空結果不是查詢本身回的.
	if _, err := repo.FindEnabledByTypeAndRegions(context.Background(), TypeWeatherWarning, []string{"台北市"}); err == nil {
		t.Error("a real query against an unreachable MongoDB must fail")
	}
}

// [F24-96] 訂閱寫入失敗的 log 不能含 userID（sub.ID = {userID}_{alertType}）.
func TestUpsertFailureLogsNoUserID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100&connectTimeoutMS=100"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	const userID = "Uaaaaaaaabbbbbbbbccccccccdddddddd"
	repo := NewAlertSubRepository(client.Database("test"))
	sub := &AlertSubscription{ID: userID + "_earthquake", UserID: userID, AlertType: "earthquake", Enabled: true}
	if err := repo.Upsert(context.Background(), sub); err == nil {
		t.Fatal("upsert against an unreachable MongoDB must fail")
	}

	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if rec["msg"] != "alertsub upsert failed" {
			continue
		}
		found = true
		if _, has := rec["id"]; has {
			t.Errorf("id field must not be logged: %v", rec)
		}
		for k, v := range rec {
			enc, mErr := json.Marshal(v)
			if mErr != nil {
				t.Fatal(mErr)
			}
			if strings.Contains(string(enc), userID) {
				t.Errorf("field %q leaks the userID: %v", k, v)
			}
		}
		if rec["alertType"] != "earthquake" {
			t.Errorf("alertType = %v", rec["alertType"])
		}
	}
	if !found {
		t.Fatalf("no 'alertsub upsert failed' record in %q", buf.String())
	}
}
