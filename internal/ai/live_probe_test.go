//go:build liveeval

package ai

// 模型探測：逐一呼叫每家帳號能用的候選模型，量能不能叫、要幾秒、中文答得像不像，
// 最後印出可以直接貼進 Render 的 *_MODELS 清單。平常的 go test 與 CI 不會編進來；手動執行：
//   NVIDIA_API_KEY_FILE=<金鑰檔> GROQ_API_KEY_FILE=<金鑰檔> GEMINI_API_KEY_FILE=<金鑰檔> \
//     go test -tags liveeval ./internal/ai/ -run TestLiveProbeModels -v -count=1 -timeout 30m
// 選用：PROBE_IMAGE=<食物照片路徑>（預設用 internal/richmenu/richmenu.png 只驗能不能看圖）、
// PROBE_MAX=每家每種能力最多試幾個（預設 15）、PROBE_TIMEOUT_SECONDS=單次上限（預設 30）。
// 金鑰只在程式內使用，不印出；一把都沒有直接失敗.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

const (
	probeListMax        = 10
	probeTextKeep       = 7
	probeVisionKeep     = 3
	probeDefaultMax     = 15
	probeDefaultTimeout = 30 * time.Second
)

type probeResult struct {
	provider string
	vision   bool
	model    string
	ok       bool
	took     time.Duration
	detail   string
}

var probeFence = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")

func TestLiveProbeModels(t *testing.T) {
	client := &http.Client{Timeout: 90 * time.Second}
	providers := []Provider{
		NewNVIDIA(probeKey(t, "NVIDIA_API_KEY_FILE"), client),
		NewGroq(probeKey(t, "GROQ_API_KEY_FILE"), client),
		NewGemini(probeKey(t, "GEMINI_API_KEY_FILE"), client),
	}
	maxPer := probeEnvInt("PROBE_MAX", probeDefaultMax)
	timeout := time.Duration(probeEnvInt("PROBE_TIMEOUT_SECONDS", int(probeDefaultTimeout/time.Second))) * time.Second
	image := probeImage(t)

	var all []probeResult
	active := 0
	for _, p := range providers {
		if p == nil {
			continue
		}
		active++
		okCount := 0
		for _, c := range []Capability{Text, Vision} {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			cands, err := p.Candidates(ctx, c)
			cancel()
			if err != nil {
				t.Errorf("%s %s 列模型失敗: %s", p.Name(), c, shortError(p.Name(), "-", err))
				continue
			}
			if len(cands) > maxPer {
				cands = cands[:maxPer]
			}
			t.Logf("%s %s 候選 %d 個: %s", p.Name(), c, len(cands), strings.Join(cands, ", "))
			for _, m := range cands {
				r := probeOne(p, m, c == Vision, image, timeout)
				if r.ok {
					okCount++
				}
				t.Logf("%-7s %-6s %-5v %5.1fs %s  %s", r.provider, c, r.ok, r.took.Seconds(), r.model, r.detail)
				all = append(all, r)
				time.Sleep(time.Second) // 免費方案每分鐘有次數上限，探測不要擠在一起.
			}
		}
		if okCount == 0 {
			t.Errorf("%s 有金鑰卻一個模型都叫不到", p.Name())
		}
	}
	if active == 0 {
		t.Fatal("沒有任何 AI 金鑰：設定 NVIDIA_API_KEY_FILE / GROQ_API_KEY_FILE / GEMINI_API_KEY_FILE 指向金鑰檔")
	}
	probeReport(t, all)
}

// probeOne 用一個貼近正式用途的題目呼叫一次：文字估算回 JSON，看圖回一句繁中描述.
func probeOne(p Provider, model string, vision bool, image Image, timeout time.Duration) probeResult {
	r := probeResult{provider: p.Name(), vision: vision, model: model}
	req := &Request{
		System: "你是台灣的營養估算助理，一律用繁體中文。",
		Prompt: `估算一碗小碗台式滷肉飯的熱量，只輸出一個 JSON 物件：{"kcal": 數字, "note": "繁體中文一句估算依據"}`,
		JSON:   true,
	}
	if vision {
		req = &Request{Prompt: "用繁體中文一句話描述這張圖片裡有什麼。", Images: []Image{image}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	text, err := p.Call(ctx, model, req)
	r.took = time.Since(start)
	if err != nil {
		r.detail = shortError(p.Name(), model, err)
		return r
	}
	text = strings.TrimSpace(stripReasoning(text))
	if vision {
		r.ok = hasCJK(text)
		r.detail = probeClip(text)
		return r
	}
	r.ok, r.detail = probeCheckEstimate(text)
	return r
}

// probeCheckEstimate 回答要是 JSON、kcal 落在小碗滷肉飯合理範圍、說明是中文.
func probeCheckEstimate(text string) (ok bool, detail string) {
	if m := probeFence.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	var v struct {
		Kcal json.Number `json:"kcal"`
		Note string      `json:"note"`
	}
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return false, "不是 JSON: " + probeClip(text)
	}
	kcal, err := v.Kcal.Float64()
	if err != nil || kcal < 150 || kcal > 900 {
		return false, fmt.Sprintf("kcal 不合理(%s): %s", v.Kcal, probeClip(v.Note))
	}
	if !hasCJK(v.Note) {
		return false, "說明不是中文: " + probeClip(v.Note)
	}
	return true, fmt.Sprintf("%.0f kcal %s", kcal, probeClip(v.Note))
}

// probeReport 每家挑能用的：文字依速度取前 7、看圖取前 3，合併去重最多 10 個，印成 Render 可貼的格式.
func probeReport(t *testing.T, all []probeResult) {
	t.Helper()
	byProvider := map[string][]probeResult{}
	for _, r := range all {
		if r.ok {
			byProvider[r.provider] = append(byProvider[r.provider], r)
		}
	}
	var slowest time.Duration
	t.Log("===== 建議貼進 Render 的值（依速度排序，可自行調整順序）=====")
	for _, name := range []string{ProviderNVIDIA, ProviderGroq, ProviderGemini} {
		rs := byProvider[name]
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].took < rs[j].took })
		var list []string
		seen := map[string]bool{}
		add := func(wantVision bool, keep int) {
			n := 0
			for _, r := range rs {
				if r.vision != wantVision || seen[r.model] || n >= keep || len(list) >= probeListMax {
					continue
				}
				seen[r.model] = true
				list = append(list, r.model)
				n++
				if !wantVision && r.took > slowest {
					slowest = r.took
				}
			}
		}
		add(false, probeTextKeep)
		add(true, probeVisionKeep)
		t.Logf("%s_MODELS=%s", strings.ToUpper(name), strings.Join(list, ","))
		for _, r := range rs {
			if r.vision {
				t.Logf("  （%s 可看圖：%s）", name, r.model)
			}
		}
	}
	if slowest > 0 {
		t.Logf("清單內文字模型最慢 %.1f 秒；AI_ATTEMPT_TIMEOUT_SECONDS 建議不低於 %d",
			slowest.Seconds(), int(math.Ceil(slowest.Seconds()*1.5)))
	}
}

func probeKey(t *testing.T, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("%s 指到的檔案讀不到（不印內容）: %v", env, err)
	}
	return strings.TrimSpace(string(b))
}

func probeImage(t *testing.T) Image {
	t.Helper()
	path := os.Getenv("PROBE_IMAGE")
	if path == "" {
		path = filepath.Join("..", "richmenu", "richmenu.png")
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("讀不到探測用圖片 %s: %v", path, err)
	}
	mime := http.DetectContentType(b)
	return Image{MIME: mime, Data: b}
}

func probeEnvInt(env string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(env)); err == nil && v > 0 {
		return v
	}
	return def
}

func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func probeClip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}
