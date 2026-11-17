package meal

import "math"

// 聊天修改紀錄的熱量範圍；單一品項上限與 nutrition 對單品的上限同值，整餐上限 6000.
const (
	MinEditCalories  = 1
	MaxItemCalories  = 3000
	MaxTotalCalories = 6000
)

// scaledTo 回傳把熱量定為 kcal、三大營養素同比例縮放的營養素；原熱量 ≤ 0 時營養素不動（沒有比例可依）.
func (n Nutrients) scaledTo(kcal float64) Nutrients {
	out := n
	if n.Calories > 0 {
		f := kcal / n.Calories
		out.ProteinG = math.Round(n.ProteinG*f*10) / 10
		out.CarbsG = math.Round(n.CarbsG*f*10) / 10
		out.FatG = math.Round(n.FatG*f*10) / 10
	}
	out.Calories = kcal
	return out
}

// editKcal 取整並檢查是否為 [MinEditCalories, hi] 內的有限數.
func editKcal(kcal, hi float64) (float64, bool) {
	if math.IsNaN(kcal) || math.IsInf(kcal, 0) {
		return 0, false
	}
	k := math.Round(kcal)
	if k < MinEditCalories || k > hi {
		return 0, false
	}
	return k, true
}

// SetItemCalories [F33-13] 把第 i 個品項（0 起算）的熱量改成 kcal（取整、1–3000），營養素同比例縮放，其餘品項不動，重算合計.
// 先 Rebase：之後的份量倍率以這個新值為基準. 範圍外或品項不存在回 false 且不改動.
func (m *Meal) SetItemCalories(i int, kcal float64) bool {
	k, ok := editKcal(kcal, MaxItemCalories)
	if !ok || i < 0 || i >= len(m.Items) {
		return false
	}
	m.Rebase()
	m.Items[i].Nutrients = m.Items[i].scaledTo(k)
	m.Recalc()
	return true
}

// SetTotalCalories [F33-13] 把整餐熱量改成 kcal（取整、1–6000）：各品項同比例縮放，進位餘數補在熱量最大的品項，
// 使 Total.Calories 精確等於 kcal. 沒有品項、範圍外回 false 且不改動.
func (m *Meal) SetTotalCalories(kcal float64) bool {
	k, ok := editKcal(kcal, MaxTotalCalories)
	if !ok || len(m.Items) == 0 {
		return false
	}
	var cur float64
	largest := 0
	for i := range m.Items {
		cur += m.Items[i].Calories
		if m.Items[i].Calories > m.Items[largest].Calories {
			largest = i
		}
	}
	targets := make([]float64, len(m.Items))
	if cur <= 0 {
		targets[0] = k // 舊資料沒有品項熱量：整餐熱量記在第一個品項.
	} else {
		var sum float64
		for i := range m.Items {
			targets[i] = math.Round(m.Items[i].Calories * k / cur)
			sum += targets[i]
		}
		targets[largest] += k - sum
		if targets[largest] < 0 {
			return false
		}
	}
	for i := range m.Items {
		m.Items[i].Nutrients = m.Items[i].scaledTo(targets[i])
	}
	m.Rebase()
	return true
}
