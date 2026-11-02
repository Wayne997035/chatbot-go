package nutrition

import (
	"chatbot-go/internal/storage/database/meal"
	"fmt"
	"strings"
)

// Disclaimer 營養數值是估算.
const Disclaimer = "（AI 估算，僅供參考）"

// FormatRecorded 格式化剛記錄的一餐；header 是第一行（例如「🍜 已記錄午餐」）.
func FormatRecorded(m *meal.Meal, header string) string {
	var sb strings.Builder
	sb.WriteString(header + "\n")
	for i := range m.Items {
		f := &m.Items[i]
		portion := ""
		if f.Portion != "" {
			portion = " " + f.Portion
		}
		fmt.Fprintf(&sb, "• %s%s：%.0f 大卡\n  %s\n", f.Name, portion, f.Calories, macros(&f.Nutrients))
	}
	fmt.Fprintf(&sb, "合計：%.0f 大卡｜%s", m.Total.Calories, macros(&m.Total))
	if s := m.CurrentScale(); s != 1 {
		fmt.Fprintf(&sb, "（份量 ×%s）", trimFloat(s))
	}
	if place := PlaceLabel(m.Place); place != "" {
		sb.WriteString("\n📍 " + place)
	}
	if m.Advice != "" {
		sb.WriteString("\n建議：" + m.Advice)
	}
	return sb.String()
}

// PlaceLabel 店家的顯示文字；推測來的會標明來源.
func PlaceLabel(p *meal.Place) string {
	if p == nil {
		return ""
	}
	label := p.Name
	if label == "" {
		label = p.Address
	}
	if label == "" {
		return ""
	}
	switch p.Source {
	case meal.PlaceFromDish:
		label += "（從餐點推測）"
	case meal.PlaceFromPhoto:
		label += "（從照片辨識）"
	}
	return label
}

// FormatDay 格式化一天的飲食總覽.
func FormatDay(meals []meal.Meal) string {
	if len(meals) == 0 {
		return "今天還沒有飲食紀錄，拍照或打字告訴我吃了什麼就能記錄喔！"
	}
	var sb strings.Builder
	total := meal.Sum(meals)
	fmt.Fprintf(&sb, "📒 今日飲食（%d 餐）%s\n", len(meals), Disclaimer)
	for i := range meals {
		m := &meals[i]
		names := make([]string, 0, len(m.Items))
		for j := range m.Items {
			names = append(names, m.Items[j].Name)
		}
		where := ""
		if m.Place != nil && m.Place.Name != "" {
			where = "（" + m.Place.Name + "）"
		}
		fmt.Fprintf(&sb, "%s %s%s：%.0f 大卡\n", m.EatenAt.In(Taipei).Format("15:04"), strings.Join(names, "、"), where, m.Total.Calories)
	}
	fmt.Fprintf(&sb, "合計：%.0f 大卡｜%s", total.Calories, macros(&total))
	return sb.String()
}

func macros(n *meal.Nutrients) string {
	return fmt.Sprintf("蛋白質 %sg｜碳水 %sg｜脂肪 %sg", trimFloat(n.ProteinG), trimFloat(n.CarbsG), trimFloat(n.FatG))
}

func trimFloat(v float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}
