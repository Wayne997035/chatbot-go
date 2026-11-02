// Package storagetest 測試用的記憶體儲存庫，行為和 MongoDB 實作一致：
// 同一組契約測試（contract.go）同時對這裡的實作與 MongoDB（整合測試）執行，
// 所以各套件的測試不必各寫一份假的，也不會因為假的行為和真的不同而出現「假綠燈」.
//
// 測試可以直接讀寫 Items 準備資料或檢查結果；並行使用時請透過方法存取.
package storagetest

import (
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Meals 記憶體版的 meal.MealRepository.
type Meals struct {
	mu     sync.Mutex
	nextID int

	Items []meal.Meal
	// BeforeUpdate 每次 Update 寫入前呼叫（不持有鎖），模擬另一個請求剛好先改了這筆.
	BeforeUpdate func(m *Meals)
	// Updates Update 被呼叫的次數.
	Updates int
}

var _ meal.MealRepository = (*Meals)(nil)

// EditStored 模擬別的請求修改第 i 筆並寫回（版本加 1）.
func (s *Meals) EditStored(i int, change func(*meal.Meal)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.Items[i])
	s.Items[i].Version++
}

// Insert 新增；沒有 ID 時產生，沒有建立時間時補上現在.
func (s *Meals) Insert(_ context.Context, m *meal.Meal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		s.nextID++
		m.ID = fmt.Sprintf("m%d", s.nextID)
	}
	if m.CreateTime.IsZero() {
		m.CreateTime = time.Now()
	}
	s.Items = append(s.Items, cloneMeal(m))
	return nil
}

// FindByUserBetween [from, to) 期間的紀錄，依用餐時間排序.
func (s *Meals) FindByUserBetween(_ context.Context, userID string, from, to time.Time) ([]meal.Meal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []meal.Meal
	for i := range s.Items {
		if m := &s.Items[i]; m.UserID == userID && !m.EatenAt.Before(from) && m.EatenAt.Before(to) {
			out = append(out, cloneMeal(m))
		}
	}
	slices.SortStableFunc(out, func(a, b meal.Meal) int { return a.EatenAt.Compare(b.EatenAt) })
	return out, nil
}

// DeleteLatest 刪除 since 之後最後「記錄」的一筆（依建立時間）.
func (s *Meals) DeleteLatest(_ context.Context, userID string, since time.Time) (*meal.Meal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := -1
	for i := range s.Items {
		m := &s.Items[i]
		if m.UserID != userID || m.CreateTime.Before(since) {
			continue
		}
		if latest < 0 || !m.CreateTime.Before(s.Items[latest].CreateTime) {
			latest = i
		}
	}
	if latest < 0 {
		return nil, nil
	}
	deleted := cloneMeal(&s.Items[latest])
	s.Items = slices.Delete(s.Items, latest, latest+1)
	return &deleted, nil
}

// DeleteAllByUser 刪除使用者所有紀錄（別人的不動）.
func (s *Meals) DeleteAllByUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Items = slices.DeleteFunc(s.Items, func(m meal.Meal) bool { return m.UserID == userID })
	return nil
}

// DeleteByID 刪除使用者自己的一筆.
func (s *Meals) DeleteByID(_ context.Context, userID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.Items, func(m meal.Meal) bool { return m.ID == id && m.UserID == userID })
	if i < 0 {
		return false, nil
	}
	s.Items = slices.Delete(s.Items, i, i+1)
	return true, nil
}

// FindByID 使用者自己的一筆；回傳複本.
func (s *Meals) FindByID(_ context.Context, userID, id string) (*meal.Meal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Items {
		if s.Items[i].ID == id && s.Items[i].UserID == userID {
			m := cloneMeal(&s.Items[i])
			return &m, nil
		}
	}
	return nil, nil
}

// Update 和 MongoDB 版一樣比對版本：讀出之後被改過就回傳 meal.ErrConflict；成功時版本加 1.
func (s *Meals) Update(_ context.Context, m *meal.Meal) (bool, error) {
	s.mu.Lock()
	s.Updates++
	hook := s.BeforeUpdate
	s.mu.Unlock()
	if hook != nil {
		hook(s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.Items, func(x meal.Meal) bool { return x.ID == m.ID && x.UserID == m.UserID })
	if i < 0 {
		return false, nil
	}
	if s.Items[i].Version != m.Version {
		return false, meal.ErrConflict
	}
	m.Version++
	s.Items[i] = cloneMeal(m)
	return true, nil
}

// cloneMeal 深層複製（品項、份量基準、店家），呼叫端修改複本不會影響儲存的資料.
func cloneMeal(m *meal.Meal) meal.Meal {
	c := *m
	c.Items = slices.Clone(m.Items)
	for i := range c.Items {
		if b := c.Items[i].Base; b != nil {
			base := *b
			c.Items[i].Base = &base
		}
	}
	if m.Place != nil {
		place := *m.Place
		c.Place = &place
	}
	return c
}

// Workouts 記憶體版的 workout.WorkoutRepository.
type Workouts struct {
	mu     sync.Mutex
	nextID int

	Items []workout.Workout
}

var _ workout.WorkoutRepository = (*Workouts)(nil)

// Insert 新增；沒有 ID 時產生.
func (s *Workouts) Insert(_ context.Context, w *workout.Workout) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.ID == "" {
		s.nextID++
		w.ID = fmt.Sprintf("w%d", s.nextID)
	}
	s.Items = append(s.Items, *w)
	return nil
}

// FindByUserBetween [from, to) 期間的紀錄，依運動時間排序.
func (s *Workouts) FindByUserBetween(_ context.Context, userID string, from, to time.Time) ([]workout.Workout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []workout.Workout
	for i := range s.Items {
		if w := s.Items[i]; w.UserID == userID && !w.DoneAt.Before(from) && w.DoneAt.Before(to) {
			out = append(out, w)
		}
	}
	slices.SortStableFunc(out, func(a, b workout.Workout) int { return a.DoneAt.Compare(b.DoneAt) })
	return out, nil
}

// DeleteLatest 刪除 since 之後運動時間最晚的一筆.
func (s *Workouts) DeleteLatest(_ context.Context, userID string, since time.Time) (*workout.Workout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := -1
	for i := range s.Items {
		w := &s.Items[i]
		if w.UserID != userID || w.DoneAt.Before(since) {
			continue
		}
		if latest < 0 || !w.DoneAt.Before(s.Items[latest].DoneAt) {
			latest = i
		}
	}
	if latest < 0 {
		return nil, nil
	}
	deleted := s.Items[latest]
	s.Items = slices.Delete(s.Items, latest, latest+1)
	return &deleted, nil
}

// DeleteAllByUser 刪除使用者所有紀錄.
func (s *Workouts) DeleteAllByUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Items = slices.DeleteFunc(s.Items, func(w workout.Workout) bool { return w.UserID == userID })
	return nil
}

// DeleteByID 刪除使用者自己的一筆.
func (s *Workouts) DeleteByID(_ context.Context, userID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.Items, func(w workout.Workout) bool { return w.ID == id && w.UserID == userID })
	if i < 0 {
		return false, nil
	}
	s.Items = slices.Delete(s.Items, i, i+1)
	return true, nil
}

// Profiles 記憶體版的 profile.ProfileRepository.
type Profiles struct {
	mu sync.Mutex

	Items map[string]profile.Profile
	// BeforeWrite 每次 Upsert / Merge 寫入前呼叫（不持有鎖），模擬另一則訊息的寫入剛好先完成.
	BeforeWrite func(p *Profiles)
}

var _ profile.ProfileRepository = (*Profiles)(nil)

// NewProfiles 建立空的身體資料儲存庫.
func NewProfiles() *Profiles { return &Profiles{Items: map[string]profile.Profile{}} }

// Get 找不到時回傳 nil, nil.
func (s *Profiles) Get(_ context.Context, userID string) (*profile.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.Items[userID]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

// Upsert 整筆取代.
func (s *Profiles) Upsert(_ context.Context, p *profile.Profile) error {
	s.beforeWrite()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items()[p.UserID] = *p
	return nil
}

// Merge 和 MongoDB 的 $set 一樣：只覆寫 patch 裡有值的欄位，沒有資料時建立.
func (s *Profiles) Merge(_ context.Context, patch *profile.Profile) (*profile.Profile, error) {
	s.beforeWrite()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.items()[patch.UserID]
	p.UserID = patch.UserID
	if patch.WeightKG > 0 {
		p.WeightKG = patch.WeightKG
	}
	if patch.HeightCM > 0 {
		p.HeightCM = patch.HeightCM
	}
	if patch.Age > 0 {
		p.Age = patch.Age
	}
	if patch.Sex != "" {
		p.Sex = patch.Sex
	}
	p.UpdateTime = patch.UpdateTime
	s.Items[patch.UserID] = p
	return &p, nil
}

// Delete 刪除身體資料.
func (s *Profiles) Delete(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items(), userID)
	return nil
}

func (s *Profiles) beforeWrite() {
	s.mu.Lock()
	hook := s.BeforeWrite
	s.mu.Unlock()
	if hook != nil {
		hook(s)
	}
}

// items 呼叫端需持有鎖.
func (s *Profiles) items() map[string]profile.Profile {
	if s.Items == nil {
		s.Items = map[string]profile.Profile{}
	}
	return s.Items
}
