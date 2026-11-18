package recordedit

// Outcome.Reason 的穩定前綴（程式產生、不含使用者文字，只給 log 與呼叫端分流用；冒號後面是可顯示的範圍提示）.
// [F33-15] OutcomeInvalid 的原因：呼叫端依前綴決定回「沒看懂」「本來就是這樣」或「這個數字看起來不太對」.
const (
	ReasonBadPlan    = "bad_plan"     // AI 沒給出可解析的計畫
	ReasonNoChange   = "no_change"    // 提議值與原值相同
	ReasonOutOfRange = "out_of_range" // 數值超範圍；格式「out_of_range: <範圍提示>」
	ReasonLastItem   = "last_item"    // 只剩一個品項不能刪
	ReasonSameMeal   = "same_meal"    // [F34-12] 整筆改餐別但本來就是那個餐別
)
