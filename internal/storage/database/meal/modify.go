package meal

import (
	"context"
	"errors"
)

// ErrConflict 寫回時這筆紀錄已經被別的請求改過（版本不符）.
var ErrConflict = errors.New("meal was modified concurrently")

// maxModifyTries 版本衝突時最多讀寫幾次.
const maxModifyTries = 3

// Editor 讀取並寫回單筆紀錄（Modify 需要的部分）.
type Editor interface {
	FindByID(ctx context.Context, userID, id string) (*Meal, error)
	Update(ctx context.Context, m *Meal) (bool, error)
}

// Modify 讀出使用者自己的一筆紀錄、套用 change 再寫回. 讀出之後被別的請求改過（例如 LIFF 改了份量），
// 就重讀並在最新的資料上重新套用，最多 maxModifyTries 次；仍然衝突時回傳 ErrConflict.
// 紀錄不存在、不屬於該使用者或期間被刪除時回傳 nil, nil. change 可能被呼叫多次，不能有其他副作用.
func Modify(ctx context.Context, store Editor, userID, id string, change func(*Meal)) (*Meal, error) {
	for range maxModifyTries {
		m, err := store.FindByID(ctx, userID, id)
		if err != nil || m == nil {
			return nil, err
		}
		change(m)
		updated, err := store.Update(ctx, m)
		switch {
		case errors.Is(err, ErrConflict):
			continue
		case err != nil:
			return nil, err
		case !updated:
			return nil, nil
		}
		return m, nil
	}
	return nil, ErrConflict
}
