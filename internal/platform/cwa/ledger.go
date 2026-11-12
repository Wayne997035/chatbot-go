package cwa

import (
	"chatbot-go/internal/platform/config"
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

var errContractStub = errors.New("cwa: contract stub")

// Ledger 用量記帳與額度判斷（F30-33、F30-34、F30-35）. 契約階段為空殼.
type Ledger struct {
	rdb *redis.Client
	s   config.CWASettings
	loc *time.Location
	now func() time.Time
}

// NewLedger 建立 Ledger；rdb 為 nil 時只用記憶體.
func NewLedger(rdb *redis.Client, s config.CWASettings, loc *time.Location, now func() time.Time) *Ledger {
	return &Ledger{rdb: rdb, s: s, loc: loc, now: now}
}

// Usage 過去 24 小時用量（唯讀）. 契約空殼：回零值.
func (l *Ledger) Usage(_ context.Context) Usage { return Usage{} }

// reserve 契約空殼：一律回錯，NEVER 回 nil.
func (l *Ledger) reserve(_ context.Context, _ CallSpec, _ int) error { return errContractStub }
