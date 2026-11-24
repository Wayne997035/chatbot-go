// Package dblog [F37-08] 每次 Mongo 寫入一行 log：集合、動作、成功與否；失敗為 ERROR 且只記錯誤類別.
// Mongo 的錯誤訊息會帶過濾值（例 dup key: { _id: "U..." }），所以 NEVER 記 err.Error()；
// 也不記 userId、名稱、email、訊息原文、文件 id.
package dblog

import (
	"chatbot-go/internal/reqid"
	"context"
	"errors"
	"log/slog"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// 動作（closed set）.
const (
	ActionInsert     = "insert"
	ActionUpdate     = "update"
	ActionUpsert     = "upsert"
	ActionReplace    = "replace"
	ActionDelete     = "delete"
	ActionDeleteMany = "delete_many"
)

// 錯誤類別（closed set）.
const (
	ClassDuplicateKey = "duplicate_key"
	ClassTimeout      = "timeout"
	ClassCanceled     = "canceled"
	ClassOther        = "other"
)

// Write 記一次寫入結果；err 為 nil 記 INFO，否則記 ERROR（帶 error_class）. 不改變呼叫端的結果或錯誤.
// nil ctx 不 panic:slog 與 reqid.From 都自行處理 nil.
func Write(ctx context.Context, collection, action string, err error) {
	if err == nil {
		slog.InfoContext(ctx, "db write", "collection", collection, "action", action, "ok", true, reqid.Attr(ctx))
		return
	}
	slog.ErrorContext(ctx, "db write", "collection", collection, "action", action, "ok", false, "error_class", Class(err), reqid.Attr(ctx))
}

// Class 把錯誤歸入封閉類別，不含錯誤訊息.
func Class(err error) string {
	switch {
	case err == nil:
		return ""
	case mongo.IsDuplicateKeyError(err):
		return ClassDuplicateKey
	case errors.Is(err, context.DeadlineExceeded) || mongo.IsTimeout(err):
		return ClassTimeout
	case errors.Is(err, context.Canceled):
		return ClassCanceled
	default:
		return ClassOther
	}
}
