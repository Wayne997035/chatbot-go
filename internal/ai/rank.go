package ai

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// family 規則：模型 id 的 model 部分（"org/" 之後）包含哪個字串就屬於哪一族，越前面越優先.
type rankRules struct {
	families []string
	skip     []string
	// require 非空時，模型必須包含其中之一（用來挑看圖模型）.
	require []string
}

var sizePattern = regexp.MustCompile(`(\d+(?:\.\d+)?)b\b`)

// rankModels 濾掉不適用的模型，依族系優先、參數量大者優先排序.
func rankModels(ids []string, rules *rankRules) []string {
	type scored struct {
		id     string
		family int
		size   float64
	}
	var out []scored
	for _, id := range ids {
		name := strings.ToLower(id[strings.LastIndex(id, "/")+1:])
		if containsAny(name, rules.skip) {
			continue
		}
		if len(rules.require) > 0 && !containsAny(name, rules.require) {
			continue
		}
		family := len(rules.families)
		for i, f := range rules.families {
			if strings.Contains(name, f) {
				family = i
				break
			}
		}
		out = append(out, scored{id: id, family: family, size: largestSize(name)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].family != out[j].family {
			return out[i].family < out[j].family
		}
		if out[i].size != out[j].size {
			return out[i].size > out[j].size
		}
		return out[i].id < out[j].id
	})
	ranked := make([]string, len(out))
	for i, s := range out {
		ranked[i] = s.id
	}
	return ranked
}

func largestSize(name string) float64 {
	var largest float64
	for _, m := range sizePattern.FindAllStringSubmatch(name, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > largest {
			largest = v
		}
	}
	return largest
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

var geminiVersion = regexp.MustCompile(`gemini-(\d+(?:\.\d+)?)`)

// geminiSkip 不是通用 Flash 文字/看圖模型的變體.
var geminiSkip = []string{"lite", "image", "tts", "live", "audio", "embedding", "native", "robotics", "computer-use", "thinking"}

// geminiFallbackAlias 列模型失敗時使用的別名，永遠指向目前的 Flash.
const geminiFallbackAlias = "gemini-flash-latest"

// rankGemini 只留 Flash（都支援看圖），穩定版優先，版本新的優先.
func rankGemini(names []string) []string {
	type scored struct {
		name     string
		unstable bool
		version  float64
	}
	var out []scored
	for _, name := range names {
		lower := strings.ToLower(name)
		if !strings.Contains(lower, "flash") || containsAny(lower, geminiSkip) {
			continue
		}
		m := geminiVersion.FindStringSubmatch(lower)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		out = append(out, scored{name: name, unstable: strings.Contains(lower, "preview") || strings.Contains(lower, "exp"), version: v})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].unstable != out[j].unstable {
			return !out[i].unstable
		}
		if out[i].version != out[j].version {
			return out[i].version > out[j].version
		}
		return len(out[i].name) < len(out[j].name)
	})
	ranked := make([]string, 0, len(out)+1)
	for _, s := range out {
		ranked = append(ranked, s.name)
	}
	return append(ranked, geminiFallbackAlias)
}
