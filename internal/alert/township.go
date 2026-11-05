package alert

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/region"
	"log/slog"
	"strings"
	"time"
)

// 有鄉鎮明細的天氣特報資料集（與部署環境無關，寫成常數）.
const (
	datasetRain = "W-C0033-003" // 豪大雨
	datasetCold = "W-C0033-004" // 低溫
	datasetWind = "W-C0033-006" // 陸上強風
)

// phenomenonCold 低溫特報的現象名稱（明細 004 的 alert_title 為「低溫特報」）.
const phenomenonCold = "低溫"

// phenomenonDataset W-C0033-001 的現象對應到哪一份鄉鎮明細；空字串表示沒有明細，照縣市通知.
// [F24-99] 001 只有颱風、豪(大)雨、陸上強風、濃霧（產品說明 6-3-1）；字樣與官方 swagger 的 phenomena
// 可填值一致（「陸上強風」另用真實回傳核對）. 低溫不在 001，由 checkColdWarnings 直接抓 004.
func phenomenonDataset(phenomena string) string {
	switch phenomena {
	case "陸上強風":
		return datasetWind
	case "大雨", "豪雨", "大豪雨", "超大豪雨":
		return datasetRain
	default:
		return ""
	}
}

// capNotice 一則有效明細在某個縣市列出的範圍.
type capNotice struct {
	level      string   // 等級字串：severity_level，沒有就用 headline
	headline   string   // 豪大雨要用它確認是不是 001 說的那種雨
	countyWide bool     // 明細直接列了整個縣市
	towns      []string // 列出的鄉鎮市區（不含縣市名）
}

// capAreas 把有效明細依縣市整理；每則有效明細在它列到的每個縣市各一筆.
// [F24-99] 不認得的地名（含以縣市名開頭、但鄉鎮不在清單內的）不算列了那個縣市，
// 呼叫端因此會照縣市通知 —— 寧可通知整縣也不靜默漏發.
func capAreas(resp *models.CWACapResponse, now time.Time) map[string][]capNotice {
	out := map[string][]capNotice{}
	unknown := 0
	for i := range resp.Records.Info {
		info := &resp.Records.Info[i]
		if !capValid(info, now) {
			continue
		}
		byCounty := map[string]*capNotice{}
		var order []string
		notice := func(county string) *capNotice {
			if n, ok := byCounty[county]; ok {
				return n
			}
			n := &capNotice{level: capLevel(info), headline: info.Headline}
			byCounty[county] = n
			order = append(order, county)
			return n
		}
		for _, a := range info.Area {
			desc := strings.ReplaceAll(a.AreaDesc, "臺", "台")
			if region.IsCounty(desc) {
				notice(desc).countyWide = true
				continue
			}
			if county, town, ok := region.SplitArea(desc); ok {
				n := notice(county)
				n.towns = append(n.towns, town)
				continue
			}
			unknown++
		}
		for _, county := range order {
			out[county] = append(out[county], *byCounty[county])
		}
	}
	if unknown > 0 {
		// 只記個數，不記地名.
		slog.Debug("unrecognized CAP areas ignored", "count", unknown)
	}
	return out
}

// capValid 有效的明細：到期時間解析得了且還沒到，而且不是解除（urgency Past 或 headline「解除」開頭）.
// [F24-99] API 會一直回最後發布的那則，實際抓到過 3 月就過期的低溫特報.
func capValid(info *models.CWACapInfo, now time.Time) bool {
	expires, err := time.Parse(time.RFC3339, info.Expires)
	if err != nil || !now.Before(expires) {
		return false
	}
	return info.Urgency != "Past" && !strings.HasPrefix(info.Headline, "解除")
}

// capLevel 等級字串：severity_level（例如「強風黃色燈號」），沒有時用 headline；不會是空字串除非兩者皆空.
func capLevel(info *models.CWACapInfo) string {
	for _, p := range info.Parameter {
		if p.ValueName == "severity_level" && p.Value != "" {
			return p.Value
		}
	}
	return info.Headline
}
