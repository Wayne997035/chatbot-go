package userstore

import "time"

// User 使用者儲存結構.
type User struct {
	ID         string    `bson:"_id"        json:"id"`
	CreateTime time.Time `bson:"createTime" json:"createTime"`
	Type       string    `bson:"type,omitempty" json:"type,omitempty"`
}
