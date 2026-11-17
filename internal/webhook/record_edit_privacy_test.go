package webhook

import (
	"chatbot-go/internal/assistant/recordedit"
	"errors"
	"strings"
	"testing"
	"time"
)

// [F33-24] A31：提議 / 確認 / 取消 / 拒絕 / 後端失敗 / 刪除資料 的 log 都不含 userID、品名、依據、原文與 nonce.
func TestEditLogsDoNotLeakContent(t *testing.T) {
	const (
		uid      = "U0123456789abcdef0123456789abcdef"
		mealName = "祕密品名牛肉乾"
		basis    = "祕密依據八十五公克"
		typed    = "獨特原文標記把熱量改成 210"
	)
	buf := captureSlog(t)
	f := newEditFixture(t)
	f.seedMeal(t, uid, mealName, 120, time.Now().Add(-time.Minute))
	plan := `{"op":"set_item_kcal","target":"M1","item":1,"kcal":210,"basis":"` + basis + `"}`
	var nonces []string

	// 提議 → 確認.
	ok := f.proposeAndGetOK(t, uid, typed, plan)
	nonces = append(nonces, nonceOf(t, ok))
	f.tapReply(t, uid, ok)
	// 提議 → 取消（確認後熱量已是 210，這次提議 250）.
	plan = `{"op":"set_item_kcal","target":"M1","item":1,"kcal":250,"basis":"` + basis + `"}`
	f.plan.answers = []string{plan}
	f.sayAs(t, uid, typed)
	no := f.buttonData(t, "no")
	nonces = append(nonces, nonceOf(t, no))
	f.tapReply(t, uid, no)
	// 壞計畫、超範圍、本來就是.
	for _, p := range []string{planGarbage, `{"op":"set_item_kcal","target":"M1","item":1,"kcal":99999,"basis":"` + basis + `"}`} {
		f.plan.answers = []string{p}
		f.sayAs(t, uid, typed)
	}
	// 後端失敗（錯誤字串內含 userID，redact 要擋下；nonce 不會出現在後端的錯誤裡，這裡不假設）.
	f.store.PutErr = errors.New("redis put failed for " + uid)
	f.plan.answers = []string{plan}
	f.sayAs(t, uid, typed)
	f.store.PutErr = nil
	f.store.TakeErr = errors.New("redis take failed for " + uid)
	f.tapReply(t, uid, "ce1:ok:"+strings.Repeat("Y", recordedit.NonceLen))
	f.store.TakeErr = nil
	// 亂 nonce 與格式錯.
	f.tap(uid, "ce1:ok:"+strings.Repeat("X", recordedit.NonceLen))
	f.tap(uid, "garbage-"+mealName)
	// 刪除資料.
	f.h.eraseUserData(t.Context(), uid)

	logs := buf.String()
	if logs == "" {
		t.Fatal("no logs captured; the test would pass vacuously")
	}
	forbidden := append([]string{
		uid, mealName, basis, "獨特原文標記", "ce1:",
		strings.Repeat("X", recordedit.NonceLen), strings.Repeat("Y", recordedit.NonceLen),
	}, nonces...)
	for _, bad := range forbidden {
		if strings.Contains(logs, bad) {
			t.Errorf("logs leak %q:\n%s", bad, logs)
		}
	}
	if !strings.Contains(logs, "record edit") {
		t.Error("expected the record edit events to be logged (class only)")
	}
}
