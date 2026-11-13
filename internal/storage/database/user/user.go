package userstore

import "time"

// User 使用者儲存結構.
//
// [F30-03] DisplayName / PictureURL 取自伺服器驗過的 LIFF ID token claims（個資）；
// 兩欄一律 json:"-"：任何序列化 User 的地方都不會外洩. 既有文件缺這兩欄 = 空字串，不需遷移.
// [F31-01] 不蒐集信箱：User 沒有對應欄位（Upsert 的 $setOnInsert 會序列化整個 User）；舊文件的殘留欄位由 SaveLineProfile 清掉.
type User struct {
	ID          string    `bson:"_id"        json:"id"`
	CreateTime  time.Time `bson:"createTime" json:"createTime"`
	Type        string    `bson:"type,omitempty" json:"type,omitempty"`
	DisplayName string    `bson:"displayName,omitempty" json:"-"`
	PictureURL  string    `bson:"pictureUrl,omitempty" json:"-"`
}
