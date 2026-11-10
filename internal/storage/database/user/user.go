package userstore

import "time"

// User 使用者儲存結構.
//
// [F30-03] DisplayName / PictureURL / Email 取自伺服器驗過的 LIFF ID token claims（個資）；
// 三欄一律 json:"-"：任何序列化 User 的地方都不會外洩. 既有文件缺這三欄 = 空字串，不需遷移.
type User struct {
	ID          string    `bson:"_id"        json:"id"`
	CreateTime  time.Time `bson:"createTime" json:"createTime"`
	Type        string    `bson:"type,omitempty" json:"type,omitempty"`
	DisplayName string    `bson:"displayName,omitempty" json:"-"`
	PictureURL  string    `bson:"pictureUrl,omitempty" json:"-"`
	Email       string    `bson:"email,omitempty" json:"-"`
}
