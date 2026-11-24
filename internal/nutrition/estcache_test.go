package nutrition

import (
	"bytes"
	"chatbot-go/internal/ai"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// estCountCompleter 數呼叫次數的假 Completer；answers 為空時一律拒絕（模擬 AI 全部失敗）.
type estCountCompleter struct {
	mu      sync.Mutex
	answers []string
	calls   int
}

func (f *estCountCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	for _, a := range f.answers {
		if req.Accept(a) {
			return ai.Result{Text: a}
		}
	}
	return ai.Result{Errors: []string{"all rejected"}}
}

func (f *estCountCompleter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const (
	estTeaAnswer = `{"is_food":true,"clarify":null,"meals":[{"meal_type":"","day_offset":0,"snack_only":true,
 "foods":[{"name":"珍珠奶茶","portion":"大杯 700ml 半糖","calories":580,"protein_g":5,"carbs_g":95,"fat_g":18}],
 "store":{"name":"某某茶店","from":"text"},"advice":"少糖少冰比較好"}]}`
	estClarifyAnswer = `{"is_food":true,"clarify":{"question":"便當差很多，是哪一種？","options":["雞腿便當","排骨便當"]},"meals":[]}`
	estTwoMeals      = `{"is_food":true,"meals":[
 {"meal_type":"lunch","foods":[{"name":"便當","calories":700}]},
 {"meal_type":"dinner","foods":[{"name":"牛肉麵","calories":650}]}]}`
	estYesterday = `{"is_food":true,"meals":[{"meal_type":"","day_offset":-1,"foods":[{"name":"雞排","calories":630}]}]}`
	estNotFood   = `{"is_food":false}`
	estSixMeals  = `{"is_food":true,"meals":[
 {"meal_type":"breakfast","foods":[{"name":"蛋餅","calories":300}]},
 {"meal_type":"lunch","foods":[{"name":"便當","calories":700}]},
 {"meal_type":"snack","foods":[{"name":"餅乾","calories":200}]},
 {"meal_type":"dinner","foods":[{"name":"牛肉麵","calories":650}]},
 {"meal_type":"late_night","foods":[{"name":"鹹酥雞","calories":500}]},
 {"meal_type":"","foods":[{"name":"珍珠奶茶","calories":580}]}]}`
)

func newEstMiniredis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

// [F37-41] 第二次同文字不呼叫 AI，結果深度相等.
func TestEstimateCacheHitSkipsAI(t *testing.T) {
	c := &estCountCompleter{answers: []string{estTeaAnswer}}
	a := NewAnalyzer(c).WithEstimateCache(NewEstimateCache(nil))
	first, err := a.EstimateText(context.Background(), "珍珠奶茶", true)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := a.EstimateText(context.Background(), " 珍珠 奶茶！", true)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if c.count() != 1 {
		t.Fatalf("[F37-41] provider calls = %d, want 1 (second call must be a cache hit)", c.count())
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("[F37-41] hit differs from first result:\nfirst  %+v\nsecond %+v", first, second)
	}
	if st := a.cache.Stats(); st.HitsLocal != 1 || st.Misses != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestEstimateCacheMissCallsAIOnceAndStores(t *testing.T) {
	mr, client := newEstMiniredis(t)
	c := &estCountCompleter{answers: []string{estTeaAnswer}}
	cache := NewEstimateCache(client)
	a := NewAnalyzer(c).WithEstimateCache(cache)
	if _, err := a.EstimateText(context.Background(), "珍珠奶茶", true); err != nil {
		t.Fatal(err)
	}
	if c.count() != 1 {
		t.Fatalf("calls = %d, want 1", c.count())
	}
	key := currentEstimateKey(true, "珍珠奶茶")
	if _, ok := cache.local.Get(key); !ok {
		t.Fatal("[F37-41] value missing from local layer")
	}
	keys := mr.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "mealest:v1:") || keys[0] != key {
		t.Fatalf("[F37-41] redis keys = %v, want exactly %q", keys, key)
	}
	if ttl := mr.TTL(key); ttl <= 6*24*time.Hour || ttl > estimateTTL {
		t.Fatalf("redis TTL = %s, want about 7 days", ttl)
	}
	// 另一個 pod（本機層是空的）從 Redis 命中，不呼叫 AI.
	other := NewAnalyzer(c).WithEstimateCache(NewEstimateCache(client))
	if _, err := other.EstimateText(context.Background(), "珍珠奶茶", true); err != nil {
		t.Fatal(err)
	}
	if c.count() != 1 || other.cache.Stats().HitsRedis != 1 {
		t.Fatalf("redis layer hit expected: calls=%d stats=%+v", c.count(), other.cache.Stats())
	}
}

func TestEstimateCacheNotStored(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		text   string
		skip   string
	}{
		{"clarify", estClarifyAnswer, "便當", skipClarify},
		{"two meals", estTwoMeals, "午餐便當晚餐牛肉麵", skipMultiMeal},
		{"six meals dropped", estSixMeals, "吃了很多", skipMultiMeal},
		{"day offset", estYesterday, "昨天吃了雞排", skipDayOffset},
		{"not food", estNotFood, "吃了閉門羹", skipNotFood},
		{"81 runes", estTeaAnswer, strings.Repeat("奶", 81), skipTooLong},
		{"21 foods", estManyFoodsAnswer(t, maxCacheFoods+1), "辦桌吃了很多菜", skipOversize},
		{"ai unavailable", "", "珍珠奶茶", skipAIError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr, client := newEstMiniredis(t)
			var answers []string
			if tt.answer != "" {
				answers = []string{tt.answer}
			}
			c := &estCountCompleter{answers: answers}
			cache := NewEstimateCache(client)
			a := NewAnalyzer(c).WithEstimateCache(cache)
			est, err := a.EstimateText(context.Background(), tt.text, true)
			if tt.skip == skipAIError {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("err = %v, want ErrUnavailable", err)
				}
			} else if err != nil || est == nil {
				t.Fatalf("result must be returned to caller: est=%+v err=%v", est, err)
			}
			if _, ok := cache.local.Get(currentEstimateKey(true, tt.text)); ok || len(mr.Keys()) != 0 {
				t.Fatalf("[F37-41] %s must not be stored (redis keys %v)", tt.name, mr.Keys())
			}
			if got := cache.Stats().Skips[tt.skip]; got != 1 {
				t.Fatalf("Skips[%s] = %d, want 1 (all: %v)", tt.skip, got, cache.Stats().Skips)
			}
			// 第二次一定再呼叫 AI.
			_, _ = a.EstimateText(context.Background(), tt.text, true)
			if c.count() != 2 {
				t.Fatalf("calls = %d, want 2 (not cached)", c.count())
			}
		})
	}
}

func TestEstimateCacheKeySeparatesClarifyMode(t *testing.T) {
	c := &estCountCompleter{answers: []string{estTeaAnswer}}
	a := NewAnalyzer(c).WithEstimateCache(NewEstimateCache(nil))
	for i, allow := range []bool{true, false, true, false} {
		if _, err := a.EstimateText(context.Background(), "珍珠奶茶", allow); err != nil {
			t.Fatal(err)
		}
		if want := map[int]int{0: 1, 1: 2, 2: 2, 3: 2}[i]; c.count() != want {
			t.Fatalf("[F37-41] step %d (allowClarify=%v): calls = %d, want %d", i, allow, c.count(), want)
		}
	}
}

func TestEstimateCacheRedisDownDegrades(t *testing.T) {
	var nilClient *redis.Client
	closed := func(t *testing.T) *redis.Client {
		mr, client := newEstMiniredis(t)
		mr.Close()
		return client
	}
	injected := func(t *testing.T) *redis.Client {
		mr, client := newEstMiniredis(t)
		mr.SetError("boom")
		return client
	}
	tests := []struct {
		name   string
		client func(t *testing.T) *redis.Client
	}{
		{"nil client", func(*testing.T) *redis.Client { return nilClient }},
		{"closed server", closed},
		{"error injected", injected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &estCountCompleter{answers: []string{estTeaAnswer}}
			a := NewAnalyzer(c).WithEstimateCache(NewEstimateCache(tt.client(t)))
			start := time.Now()
			first, err := a.EstimateText(context.Background(), "珍珠奶茶", true)
			if err != nil || first == nil || len(first.Meals) != 1 {
				t.Fatalf("redis failure must not fail the message: est=%+v err=%v", first, err)
			}
			if _, err := a.EstimateText(context.Background(), "珍珠奶茶", true); err != nil {
				t.Fatal(err)
			}
			if c.count() != 1 {
				t.Fatalf("local layer must still serve: calls = %d", c.count())
			}
			if d := time.Since(start); d > 1500*time.Millisecond {
				t.Fatalf("took %s", d)
			}
		})
	}
}

// [F37-41] 接受連線但永不回應的 Redis：客戶端與 production 同形（只給 Addr），整個未命中呼叫（GET + SET）< 1500ms.
func TestEstimateCacheRedisHangDegrades(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	client := redis.NewClient(&redis.Options{Addr: ln.Addr().String()})
	t.Cleanup(func() { _ = client.Close() })

	c := &estCountCompleter{answers: []string{estTeaAnswer}}
	a := NewAnalyzer(c).WithEstimateCache(NewEstimateCache(client))
	start := time.Now()
	est, err := a.EstimateText(context.Background(), "珍珠奶茶", true)
	elapsed := time.Since(start)
	if err != nil || est == nil || len(est.Meals) != 1 || est.Meals[0].Foods[0].Name != "珍珠奶茶" {
		t.Fatalf("hung redis must not fail the message: est=%+v err=%v", est, err)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("[F37-41] miss with hung redis took %s, want < 1500ms", elapsed)
	}
	if c.count() != 1 {
		t.Fatalf("calls = %d, want 1", c.count())
	}
}

func TestEstimateCacheCorruptEntryIsMiss(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"invalid json", `{not json`},
		{"future schema", `{"v":2,"meal":{"foods":[{"name":"x","portion":"","calories":100,"protein_g":0,"carbs_g":0,"fat_g":0}],"place":null,"meal_type":"","snack_only":false,"advice":""}}`},
		{"zero foods", `{"v":1,"meal":{"foods":[],"place":null,"meal_type":"","snack_only":false,"advice":""}}`},
		{"calories below 1", `{"v":1,"meal":{"foods":[{"name":"x","portion":"","calories":0,"protein_g":0,"carbs_g":0,"fat_g":0}],"place":null,"meal_type":"","snack_only":false,"advice":""}}`},
		{"unknown meal type", `{"v":1,"meal":{"foods":[{"name":"x","portion":"","calories":10,"protein_g":0,"carbs_g":0,"fat_g":0}],"place":null,"meal_type":"brunchy","snack_only":false,"advice":""}}`},
		{"place without source", `{"v":1,"meal":{"foods":[{"name":"x","portion":"","calories":10,"protein_g":0,"carbs_g":0,"fat_g":0}],"place":{"name":"店","source":""},"meal_type":"","snack_only":false,"advice":""}}`},
		// [PR37 SE37-03] 命中的值也走 cleanFood / CleanPlaceName：網址、沒有文字的品名不收.
		{"food name with url", `{"v":1,"meal":{"foods":[{"name":"see evil.example.com","portion":"","calories":10,"protein_g":0,"carbs_g":0,"fat_g":0}],"place":null,"meal_type":"","snack_only":false,"advice":""}}`},
		{"food name without letters", `{"v":1,"meal":{"foods":[{"name":"123","portion":"","calories":10,"protein_g":0,"carbs_g":0,"fat_g":0}],"place":null,"meal_type":"","snack_only":false,"advice":""}}`},
		{"place name with url", `{"v":1,"meal":{"foods":[{"name":"x","portion":"","calories":10,"protein_g":0,"carbs_g":0,"fat_g":0}],"place":{"name":"http://evil.example","source":"user"},"meal_type":"","snack_only":false,"advice":""}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr, client := newEstMiniredis(t)
			key := currentEstimateKey(true, "珍珠奶茶")
			if err := mr.Set(key, tt.raw); err != nil {
				t.Fatal(err)
			}
			c := &estCountCompleter{answers: []string{estTeaAnswer}}
			a := NewAnalyzer(c).WithEstimateCache(NewEstimateCache(client))
			est, err := a.EstimateText(context.Background(), "珍珠奶茶", true)
			if err != nil || est == nil || len(est.Meals) != 1 || est.Meals[0].Foods[0].Name != "珍珠奶茶" {
				t.Fatalf("[F37-41] corrupt entry must yield the AI result: est=%+v err=%v", est, err)
			}
			if c.count() != 1 {
				t.Fatalf("calls = %d, want 1", c.count())
			}
			got, gerr := mr.Get(key)
			if gerr != nil || got == tt.raw || !strings.Contains(got, "珍珠奶茶") {
				t.Fatalf("entry must be rewritten with the AI result: %q err=%v", got, gerr)
			}
		})
	}
}

// estManyFoodsAnswer 產生一餐 n 道菜的模型回答（每道都通過 cleanFood）.
func estManyFoodsAnswer(t *testing.T, n int) string {
	t.Helper()
	type food struct {
		Name     string `json:"name"`
		Calories int    `json:"calories"`
	}
	foods := make([]food, n)
	for i := range foods {
		foods[i] = food{Name: "菜" + strings.Repeat("色", i%10+1), Calories: 100 + i}
	}
	raw, err := json.Marshal(map[string]any{
		"is_food": true,
		"meals":   []map[string]any{{"meal_type": "", "day_offset": 0, "foods": foods}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// [F37-41] 編碼後超過 maxCacheValueBytes 的值不存（parser 擋得住的長度以外的最後一道）.
func TestEstimateCacheStoreRejectsOversizeValue(t *testing.T) {
	mr, client := newEstMiniredis(t)
	cache := NewEstimateCache(client)
	foods := make([]meal.FoodItem, maxCacheFoods)
	for i := range foods {
		foods[i] = meal.FoodItem{Name: strings.Repeat("飯", 200), Portion: "一碗"}
	}
	est := &TextEstimate{IsFood: true, Meals: []MealEstimate{{Foods: foods}}}
	key := currentEstimateKey(true, "便當")
	stored, skip := cache.store(context.Background(), key, "便當", est)
	if stored || skip != skipOversize {
		t.Fatalf("[F37-41] store = (%v, %q), want (false, %q)", stored, skip, skipOversize)
	}
	if _, ok := cache.local.Get(key); ok || len(mr.Keys()) != 0 {
		t.Fatalf("[F37-41] oversize value must not be stored (redis keys %v)", mr.Keys())
	}
}

// [F37-41] Redis 裡的壞資料要留一行 Warn，且不帶 key 或內容.
func TestEstimateCacheCorruptEntryWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mr, client := newEstMiniredis(t)
	key := currentEstimateKey(true, "珍珠奶茶")
	if err := mr.Set(key, `{SENTINELBAD`); err != nil {
		t.Fatal(err)
	}
	a := NewAnalyzer(&estCountCompleter{answers: []string{estTeaAnswer}}).WithEstimateCache(NewEstimateCache(client))
	if _, err := a.EstimateText(context.Background(), "珍珠奶茶", true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"meal estimate cache redis"`) || !strings.Contains(out, `"op":"decode"`) {
		t.Fatalf("[F37-41] corrupt entry must log a decode Warn:\n%s", out)
	}
	for _, banned := range []string{"SENTINELBAD", "mealest:", "珍珠奶茶"} {
		if strings.Contains(out, banned) {
			t.Errorf("[F37-41] Warn leaks %q:\n%s", banned, out)
		}
	}
}

// [PR37 SE37-03] 命中的建議也走 cleanAdvice：含網址的建議丟掉，估算照用、不呼叫 AI.
func TestEstimateCacheHitDropsAdviceWithURL(t *testing.T) {
	mr, client := newEstMiniredis(t)
	key := currentEstimateKey(true, "珍珠奶茶")
	raw := `{"v":1,"meal":{"foods":[{"name":"珍珠奶茶","portion":"","calories":580,"protein_g":0,"carbs_g":0,"fat_g":0}],"place":null,"meal_type":"","snack_only":true,"advice":"go to evil.example.com"}}`
	if err := mr.Set(key, raw); err != nil {
		t.Fatal(err)
	}
	c := &estCountCompleter{answers: []string{estTeaAnswer}}
	est, err := NewAnalyzer(c).WithEstimateCache(NewEstimateCache(client)).EstimateText(context.Background(), "珍珠奶茶", true)
	if err != nil || est == nil || len(est.Meals) != 1 {
		t.Fatalf("est=%+v err=%v", est, err)
	}
	if c.count() != 0 {
		t.Fatalf("calls = %d, want 0 (the entry is still a hit)", c.count())
	}
	if est.Meals[0].Advice != "" {
		t.Fatalf("[PR37 SE37-03] advice with a URL must be dropped on a hit, got %q", est.Meals[0].Advice)
	}
}

func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestEstimateCacheValueHasNoUserData(t *testing.T) {
	mr, client := newEstMiniredis(t)
	cache := NewEstimateCache(client)
	a := NewAnalyzer(&estCountCompleter{answers: []string{estTeaAnswer}}).WithEstimateCache(cache)
	const text = "SENTINELMEAL珍珠奶茶"
	if _, err := a.EstimateText(context.Background(), text, true); err != nil {
		t.Fatal(err)
	}
	key := currentEstimateKey(true, text)
	local, ok := cache.local.Get(key)
	if !ok {
		t.Fatal("local value missing")
	}
	localRaw, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	redisRaw, err := mr.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"local": localRaw, "redis": []byte(redisRaw)} {
		if s := string(raw); strings.Contains(s, "SENTINELMEAL") || strings.Contains(strings.ToLower(s), "userid") {
			t.Errorf("[F37-41] %s value leaks user data: %s", name, s)
		}
		if got, want := jsonKeys(t, raw), []string{"meal", "v"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s top-level fields = %v, want %v", name, got, want)
		}
		var top struct {
			Meal json.RawMessage `json:"meal"`
		}
		if err := json.Unmarshal(raw, &top); err != nil {
			t.Fatal(err)
		}
		wantMeal := []string{"advice", "foods", "meal_type", "place", "snack_only"}
		if got := jsonKeys(t, top.Meal); !reflect.DeepEqual(got, wantMeal) {
			t.Errorf("%s meal fields = %v, want %v", name, got, wantMeal)
		}
		var stored struct {
			Foods []json.RawMessage `json:"foods"`
			Place json.RawMessage   `json:"place"`
		}
		if err := json.Unmarshal(top.Meal, &stored); err != nil || len(stored.Foods) != 1 {
			t.Fatalf("meal decode: %v foods=%d", err, len(stored.Foods))
		}
		wantFood := []string{"calories", "carbs_g", "fat_g", "name", "portion", "protein_g"}
		if got := jsonKeys(t, stored.Foods[0]); !reflect.DeepEqual(got, wantFood) {
			t.Errorf("%s food fields = %v, want %v", name, got, wantFood)
		}
		wantPlace := []string{"name", "source"}
		if got := jsonKeys(t, stored.Place); !reflect.DeepEqual(got, wantPlace) {
			t.Errorf("%s place fields = %v, want %v (source is required)", name, got, wantPlace)
		}
	}
	if localRaw := string(localRaw); !strings.Contains(localRaw, `"source":"user"`) {
		t.Errorf("place.source must be stored: %s", localRaw)
	}
}

func TestEstimateCacheHitIsDeepCopy(t *testing.T) {
	a := NewAnalyzer(&estCountCompleter{answers: []string{estTeaAnswer}}).WithEstimateCache(NewEstimateCache(nil))
	if _, err := a.EstimateText(context.Background(), "珍珠奶茶", true); err != nil {
		t.Fatal(err)
	}
	hit, err := a.EstimateText(context.Background(), "珍珠奶茶", true)
	if err != nil {
		t.Fatal(err)
	}
	// 模擬 answerMealClarify 會改寫命中的估算.
	hit.Meals[0].MealType = Dinner
	hit.Meals[0].DayOffset = -1
	hit.Meals[0].Foods[0].Name = "被改過"
	hit.Meals[0].Place.Name = "被改過"
	again, err := a.EstimateText(context.Background(), "珍珠奶茶", true)
	if err != nil {
		t.Fatal(err)
	}
	m := &again.Meals[0]
	if m.MealType != "" || m.DayOffset != 0 || m.Foods[0].Name != "珍珠奶茶" || m.Place.Name != "某某茶店" {
		t.Fatalf("[F37-41] mutation of one hit leaked into the next: %+v", m)
	}
}

func TestEstimateCacheCountersAndLogs(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cache := NewEstimateCache(nil)
	food := &estCountCompleter{answers: []string{estTeaAnswer}}
	dated := &estCountCompleter{answers: []string{estYesterday}}
	down := &estCountCompleter{}
	const text = "SENTINELMEAL牛肉麵"
	steps := []struct {
		a     *Analyzer
		text  string
		allow bool
	}{
		{NewAnalyzer(food).WithEstimateCache(cache), text, true},              // miss, stored
		{NewAnalyzer(food).WithEstimateCache(cache), text, true},              // hit local
		{NewAnalyzer(dated).WithEstimateCache(cache), "昨天吃了雞排", false},        // miss, day_offset
		{NewAnalyzer(down).WithEstimateCache(cache), "U-sentinel-1 便當", true}, // miss, ai_error
	}
	for _, s := range steps {
		_, _ = s.a.EstimateText(context.Background(), s.text, s.allow)
	}
	st := cache.Stats()
	if st.HitsLocal != 1 || st.HitsRedis != 0 || st.Misses != 3 || st.Skips[skipDayOffset] != 1 || st.Skips[skipAIError] != 1 {
		t.Fatalf("[F37-41] stats = %+v", st)
	}
	out := buf.String()
	for _, banned := range []string{"SENTINELMEAL", "U-sentinel", "mealest:", "牛肉麵", "雞排"} {
		if strings.Contains(out, banned) {
			t.Errorf("[F37-41] log leaks %q:\n%s", banned, out)
		}
	}
	if regexp.MustCompile(`[0-9a-f]{16,}`).MatchString(out) {
		t.Errorf("[F37-41] log contains a hex string (hash/key):\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(steps) {
		t.Fatalf("want %d cache lines, got %d:\n%s", len(steps), len(lines), out)
	}
	wantResult := []string{"miss", "hit", "miss", "miss"}
	wantSkip := []string{"", "", skipDayOffset, skipAIError}
	wantStored := []bool{true, false, false, false}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		delete(rec, "time")
		delete(rec, "level")
		if rec["msg"] != "meal estimate cache" {
			t.Fatalf("line %d msg = %v", i, rec["msg"])
		}
		delete(rec, "msg")
		keys := make([]string, 0, len(rec))
		for k := range rec {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if want := []string{"clarify_mode", "layer", "result", "skip", "stored"}; !reflect.DeepEqual(keys, want) {
			t.Fatalf("line %d fields = %v, want %v", i, keys, want)
		}
		if rec["result"] != wantResult[i] || rec["skip"] != wantSkip[i] || rec["stored"] != wantStored[i] || rec["clarify_mode"] != steps[i].allow {
			t.Errorf("line %d = %v", i, rec)
		}
	}
}
