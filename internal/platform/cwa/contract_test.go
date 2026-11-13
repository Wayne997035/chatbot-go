package cwa_test

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// F30-30 契約：D2 的 cwa 匯出；簽名改一個字就編譯失敗.
var (
	_ func() []string                                                                        = cwa.Streams
	_ func(error) string                                                                     = cwa.Classify
	_ func(error) string                                                                     = cwa.ClassOf
	_ func(*redis.Client, *config.CWASettings, *time.Location, func() time.Time) *cwa.Ledger = cwa.NewLedger
	_ func(*cwa.Ledger, context.Context) cwa.Usage                                           = (*cwa.Ledger).Usage
	_ func(cwa.ClientOptions, *cwa.Ledger, *config.CWASettings) *cwa.Client                  = cwa.NewClient
	_ func(*cwa.Client, context.Context, cwa.CallSpec, string, url.Values, any) error        = (*cwa.Client).GetJSON
	_ func(*cwa.Client) time.Duration                                                        = (*cwa.Client).FetchBudget
	_ func(func(string) (*time.Location, error)) (*time.Location, error)                     = cwa.LoadTaipei
	_ func(*cwa.Error) string                                                                = (*cwa.Error).Error
	_ func(*cwa.Error) error                                                                 = (*cwa.Error).Unwrap
)

// typeIs pins a value type at compile time: when the field type changes this call stops compiling.
func typeIs[T any](_ T) {}

func TestContractConstants(t *testing.T) {
	typeIs[string](cwa.StreamWarning)
	typeIs[string](cwa.StreamEarthquake)
	typeIs[string](cwa.StreamTsunami)
	typeIs[string](cwa.StreamCold)
	typeIs[string](cwa.StreamForecast)
	typeIs[time.Duration](cwa.AlertAttemptTimeout)
	typeIs[time.Duration](cwa.ForecastAttemptTimeout)
	typeIs[time.Duration](cwa.RetryBackoff)
	typeIs[int](cwa.ForecastDatasetCount)
	typeIs[int](cwa.MaxBody)
	typeIs[error](cwa.ErrQuotaReserved)
	if cwa.ForecastDatasetCount != 23 {
		t.Fatal("ForecastDatasetCount must be 23")
	}
	got := cwa.Streams()
	want := []string{"warning", "earthquake", "tsunami", "cold", "forecast"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Streams() = %v", got)
		}
	}
}

func TestContractStructFields(t *testing.T) {
	e := cwa.Error{Class: "dns", Attempts: 2, Err: context.Canceled}
	typeIs[string](e.Class)
	typeIs[int](e.Attempts)
	typeIs[error](e.Err)
	cs := cwa.CallSpec{Stream: "warning", Exempt: true, Dataset: "x"}
	typeIs[string](cs.Stream)
	typeIs[bool](cs.Exempt)
	typeIs[string](cs.Dataset)
	u := cwa.Usage{Total: 1, ByStream: map[string]int{}, Limit: 1, WindowHours: 24}
	typeIs[int](u.Total)
	typeIs[map[string]int](u.ByStream)
	typeIs[int](u.Limit)
	typeIs[int](u.WindowHours)
	o := cwa.ClientOptions{
		BaseURL: "b", AuthKey: "k", HTTPClient: &http.Client{}, AttemptTimeout: 1, Backoff: 1, MaxBody: 1,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	typeIs[string](o.BaseURL)
	typeIs[string](o.AuthKey)
	typeIs[*http.Client](o.HTTPClient)
	typeIs[time.Duration](o.AttemptTimeout)
	typeIs[time.Duration](o.Backoff)
	typeIs[int64](o.MaxBody)
	typeIs[func(context.Context, time.Duration) error](o.Sleep)
	if e.Error() != "cwa fetch failed: dns (2 attempts)" {
		t.Fatalf("Error() = %q", e.Error())
	}
}
