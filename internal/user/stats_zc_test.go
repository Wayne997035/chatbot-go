package user

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// zcFullIDPattern LINE userID 的形狀：U 加 32 位 hex.
var zcFullIDPattern = regexp.MustCompile(`U[0-9a-fA-F]{32}`)

// [F30-90] TestStatsNoStoreLogsError 守住：沒有 StatsStore（部署錯誤）時回 500，並印恰好一行 "admin stats failed"，該行不含 userID 形狀的字串.
func TestStatsNoStoreLogsError(t *testing.T) {
	logs := statsCaptureLog(t)
	rec, handled := statsCall(t, NewUserHandler(nil, nil), "")
	if rec.Code != http.StatusInternalServerError || handled != 0 {
		t.Fatalf("code=%d handled=%d body=%s", rec.Code, handled, rec.Body.String())
	}
	out := logs.String()
	if n := strings.Count(out, "admin stats failed"); n != 1 {
		t.Fatalf("error log lines = %d, want 1: %s", n, out)
	}
	if !strings.Contains(out, errNoStatsStore.Error()) {
		t.Errorf("log must say why: %s", out)
	}
	if m := zcFullIDPattern.FindString(out); m != "" {
		t.Errorf("log contains an ID-shaped string %q: %s", m, out)
	}
}
