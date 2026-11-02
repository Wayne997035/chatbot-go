package nutrition

import (
	"chatbot-go/internal/storage/database/meal"
	"fmt"
	"strings"
)

// Disclaimer 營養數值是估算.
const Disclaimer = "（AI 估算，僅供參考）"

// FormatMeal 格式化一餐的分析結果.
func FormatMeal(a *ImageAnalysis) string {
	var sb strings.Builder
	sb.WriteString("🍱 餐點分析" + Disclaimer + "\n")
	for i := range a.Foods {
		f := &a.Foods[i]
		portion := ""
		if f.Portion != "" {
			portion = " " + f.Portion
		}
		fmt.Fprintf(&sb, "• %s%s：%.0f 大卡\n  %s\n", f.Name, portion, f.Calories, macros(&f.Nutrients))
	}
	fmt.Fprintf(&sb, "合計：%.0f 大卡｜%s", a.Total.Calories, macros(&a.Total))
	if a.Advice != "" {
		sb.WriteString("\n建議：" + a.Advice)
	}
	return sb.String()
}

// FormatDay 格式化一天的飲食總覽.
func FormatDay(meals []meal.Meal) string {
	if len(meals) == 0 {
		return "今天還沒有飲食紀錄，拍一張餐點照片傳給我就能記錄喔！"
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
		fmt.Fprintf(&sb, "%s %s：%.0f 大卡\n", m.EatenAt.In(Taipei).Format("15:04"), strings.Join(names, "、"), m.Total.Calories)
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
