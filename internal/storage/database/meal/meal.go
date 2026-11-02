// Package meal 飲食紀錄儲存.
package meal

import (
	"math"
	"time"
)

// Nutrients 營養素.
type Nutrients struct {
	Calories float64 `bson:"calories"`
	ProteinG float64 `bson:"proteinG"`
	CarbsG   float64 `bson:"carbsG"`
	FatG     float64 `bson:"fatG"`
}

// Add 累加營養素.
func (n *Nutrients) Add(o *Nutrients) {
	n.Calories += o.Calories
	n.ProteinG += o.ProteinG
	n.CarbsG += o.CarbsG
	n.FatG += o.FatG
}

// scale 乘上倍數；熱量取整數，克數取到小數一位.
func (n *Nutrients) scale(f float64) {
	n.Calories = math.Round(n.Calories * f)
	n.ProteinG = math.Round(n.ProteinG*f*10) / 10
	n.CarbsG = math.Round(n.CarbsG*f*10) / 10
	n.FatG = math.Round(n.FatG*f*10) / 10
}

// FoodItem 一樣食物.
type FoodItem struct {
	Name      string `bson:"name"`
	Portion   string `bson:"portion"`
	Nutrients `bson:",inline"`
	// Base 改份量前的估算值；份量一律從這裡算，來回調整也不會因為四捨五入累積誤差.
	Base *Nutrients `bson:"base,omitempty"`
}

// 紀錄來源.
const (
	SourcePhoto     = "photo"
	SourceText      = "text"
	SourcePhotoText = "photo_text" // 先打字、再補照片
)

// 店家資訊的來源；只存使用者主動提供或明顯辨識得出來的.
const (
	PlaceFromUser  = "user"  // 使用者打字說的或在地圖上選的
	PlaceFromPhoto = "photo" // 照片上看得到店名、logo、包裝或收據
	PlaceFromDish  = "dish"  // 連鎖店獨有的招牌品項（大麥克 → 麥當勞）
)

// Place 在哪裡吃的；不存經緯度.
type Place struct {
	Name    string `bson:"name,omitempty"`
	Address string `bson:"address,omitempty"`
	Source  string `bson:"source"`
}

// 份量倍數的範圍.
const (
	MinScale = 0.25
	MaxScale = 3
)

// Meal 一餐的紀錄. 不保存照片本身，只存分析結果.
//
// Items 與 Total 永遠是目前份量的數值；Scale 是相對於最近一次估算的倍數（0 視為 1）.
type Meal struct {
	ID         string     `bson:"_id"`
	UserID     string     `bson:"userId"`
	EatenAt    time.Time  `bson:"eatenAt"`
	Items      []FoodItem `bson:"items"`
	Total      Nutrients  `bson:"total"`
	Advice     string     `bson:"advice,omitempty"`
	Source     string     `bson:"source,omitempty"`
	MealType   string     `bson:"mealType,omitempty"`
	Note       string     `bson:"note,omitempty"` // 使用者打的原文
	Place      *Place     `bson:"place,omitempty"`
	Scale      float64    `bson:"scale,omitempty"`
	CreateTime time.Time  `bson:"createTime"`
	// Version 每次寫回加 1；寫回時比對，避免同時修改（例如 LIFF 改份量、聊天加品項）互相覆蓋.
	Version int64 `bson:"version"`
}

// CurrentScale 目前的份量倍數.
func (m *Meal) CurrentScale() float64 {
	if m.Scale <= 0 {
		return 1
	}
	return m.Scale
}

// ValidScale 份量倍數是否在可接受的範圍內.
func ValidScale(scale float64) bool {
	return scale >= MinScale && scale <= MaxScale && !math.IsNaN(scale)
}

// SetScale 把份量改成最近一次估算的 scale 倍（不是在目前份量上再乘）；超出範圍回傳 false.
func (m *Meal) SetScale(scale float64) bool {
	if !ValidScale(scale) {
		return false
	}
	current := m.CurrentScale()
	for i := range m.Items {
		item := &m.Items[i]
		if item.Base == nil {
			base := item.Nutrients
			base.scale(1 / current)
			item.Base = &base
		}
		item.Nutrients = *item.Base
		item.scale(scale)
	}
	m.Scale = scale
	m.Recalc()
	return true
}

// Rebase 以目前的數值當作新的估算基準（補照片重估或加點品項之後使用）.
func (m *Meal) Rebase() {
	for i := range m.Items {
		m.Items[i].Base = nil
	}
	m.Scale = 0
	m.Recalc()
}

// Recalc 依品項重新計算合計.
func (m *Meal) Recalc() {
	m.Total = Nutrients{}
	for i := range m.Items {
		m.Total.Add(&m.Items[i].Nutrients)
	}
}

// Sum 合計多餐的營養素.
func Sum(meals []Meal) Nutrients {
	var total Nutrients
	for i := range meals {
		total.Add(&meals[i].Total)
	}
	return total
}
