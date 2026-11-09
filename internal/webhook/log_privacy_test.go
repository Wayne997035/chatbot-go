package webhook

import (
	"bytes"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// errWeatherRepo 所有查詢都回錯誤的天氣 repo（fakeWeatherRepo 不會回錯誤）.
type errWeatherRepo struct{}

var errWeatherRepoDown = errors.New("weather repo down")

func (errWeatherRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	return nil, errWeatherRepoDown
}

func (errWeatherRepo) FindByDistrictAndCity(context.Context, string, string) (*weatherstore.WeatherForecast, error) {
	return nil, errWeatherRepoDown
}

func (errWeatherRepo) FindAllDistricts(context.Context) ([]weatherstore.DistrictEntry, error) {
	return nil, nil
}

func (errWeatherRepo) Upsert(context.Context, *weatherstore.WeatherForecast) error { return nil }

// captureSlog 把預設 slog 換成寫進 buffer 的 JSON handler，結束時還原.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// recordsByMsg 逐行 Unmarshal 後依 msg 篩選.
func recordsByMsg(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// [F24-95] 天氣查詢失敗的 log 不能帶使用者輸入原文與位置.
func TestWeatherLookupFailureLogsNoUserText(t *testing.T) {
	const (
		userText = "我家住在假路一百二十三號"
		city     = "假甲市"
		district = "乙丙區"
		address  = city + district + "假街1號"
		cCity    = "假乙市"
		cDist    = "丙丁區"
	)
	newHandler := func(api *lineAPI) *WebhookHandler {
		h := NewWebhookHandler(api.client, nil, weather.NewLookup(errWeatherRepo{}, nil, 60), nil, conversation.NewManager(nil))
		return h
	}
	assertClean := func(t *testing.T, rec map[string]any, forbidden ...string) {
		t.Helper()
		for _, f := range []string{"district", "city"} {
			if _, has := rec[f]; has {
				t.Errorf("%s field must not be logged: %v", f, rec)
			}
		}
		for k, v := range rec {
			enc, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range forbidden {
				if strings.Contains(string(enc), bad) {
					t.Errorf("field %q leaks %q: %v", k, bad, v)
				}
			}
		}
	}
	const wantReply = "查詢天氣資料時發生錯誤"

	t.Run("by district", func(t *testing.T) {
		buf := captureSlog(t)
		api := newLineAPI(t, nil)
		newHandler(api).replyWeatherQuery(context.Background(), "U1", userText, "tok")
		recs := recordsByMsg(t, buf, "weather lookup by district")
		if len(recs) != 1 {
			t.Fatalf("records = %d: %s", len(recs), buf.String())
		}
		assertClean(t, recs[0], userText)
		if recs[0]["query_len"] != float64(utf8.RuneCountInString(userText)) {
			t.Errorf("query_len = %v", recs[0]["query_len"])
		}
		if !strings.Contains(api.lastReply(t), wantReply) {
			t.Errorf("user must still get the error reply: %s", api.lastReply(t))
		}
	})

	t.Run("by shared location", func(t *testing.T) {
		buf := captureSlog(t)
		api := newLineAPI(t, nil)
		newHandler(api).handleLocationMessage(context.Background(), "U1", address, "tok")
		recs := recordsByMsg(t, buf, "weather lookup by location")
		if len(recs) != 1 {
			t.Fatalf("records = %d: %s", len(recs), buf.String())
		}
		assertClean(t, recs[0], city, district)
		if !strings.Contains(api.lastReply(t), wantReply) {
			t.Errorf("user must still get the error reply: %s", api.lastReply(t))
		}
	})

	t.Run("after clarify", func(t *testing.T) {
		buf := captureSlog(t)
		api := newLineAPI(t, nil)
		h := newHandler(api)
		state := &conversation.State{Candidates: []conversation.CandidateLocation{
			{City: cCity, District: cDist, DisplayName: cCity + cDist},
		}}
		h.handleLocationClarify(context.Background(), "U1", "1", "tok", state)
		recs := recordsByMsg(t, buf, "weather lookup after clarify")
		if len(recs) != 1 {
			t.Fatalf("records = %d: %s", len(recs), buf.String())
		}
		assertClean(t, recs[0], cCity, cDist)
		if !strings.Contains(api.lastReply(t), wantReply) {
			t.Errorf("user must still get the error reply: %s", api.lastReply(t))
		}
	})
}
