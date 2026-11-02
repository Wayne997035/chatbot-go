// Package meal 飲食紀錄儲存.
package meal

import "time"

// Nutrients 營養素.
type Nutrients struct {
	Calories float64 `bson:"calories" json:"calories"`
	ProteinG float64 `bson:"proteinG" json:"protein_g"`
	CarbsG   float64 `bson:"carbsG"   json:"carbs_g"`
	FatG     float64 `bson:"fatG"     json:"fat_g"`
}

// Add 累加營養素.
func (n *Nutrients) Add(o *Nutrients) {
	n.Calories += o.Calories
	n.ProteinG += o.ProteinG
	n.CarbsG += o.CarbsG
	n.FatG += o.FatG
}

// FoodItem 一樣食物.
type FoodItem struct {
	Name      string `bson:"name"    json:"name"`
	Portion   string `bson:"portion" json:"portion"`
	Nutrients `bson:",inline"`
}

// Meal 一餐（一張照片）的紀錄. 不保存照片本身，只存分析結果.
type Meal struct {
	ID         string     `bson:"_id"`
	UserID     string     `bson:"userId"`
	EatenAt    time.Time  `bson:"eatenAt"`
	Items      []FoodItem `bson:"items"`
	Total      Nutrients  `bson:"total"`
	Advice     string     `bson:"advice,omitempty"`
	CreateTime time.Time  `bson:"createTime"`
}

// Sum 合計多餐的營養素.
func Sum(meals []Meal) Nutrients {
	var total Nutrients
	for i := range meals {
		total.Add(&meals[i].Total)
	}
	return total
}
