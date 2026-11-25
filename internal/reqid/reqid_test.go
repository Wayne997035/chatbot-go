package reqid

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

var hex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)

// [F37-08] New 是 16 個小寫 hex，1000 次都不重複.
func TestNewIsSixteenLowerHexAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := New()
		if !hex16.MatchString(id) {
			t.Fatalf("New() = %q, want 16 lower-case hex", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q after %d calls", id, i)
		}
		seen[id] = true
	}
}

// [F37-08] crypto/rand 失敗時回 unknown，不 panic；With 仍可用、訊息照常處理.
func TestNewFallsBackToUnknownWhenRandFails(t *testing.T) {
	orig := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("entropy exhausted") }
	t.Cleanup(func() { randRead = orig })

	if got := New(); got != Unknown {
		t.Errorf("New() = %q, want %q", got, Unknown)
	}
	if got, ok := From(With(context.Background())); !ok || got != Unknown {
		t.Errorf("From(With(ctx)) = %q ok=%v, want %q", got, ok, Unknown)
	}
}

// [F37-08] With / From 往返、兩個 ctx 代號不同、WithoutCancel 之後仍在.
func TestWithFromRoundTrip(t *testing.T) {
	if _, ok := From(context.Background()); ok {
		t.Error("a bare context must have no request id")
	}
	a, b := With(context.Background()), With(context.Background())
	ida, _ := From(a)
	idb, _ := From(b)
	if ida == idb {
		t.Errorf("two contexts share id %q", ida)
	}
	if got, ok := From(context.WithoutCancel(a)); !ok || got != ida {
		t.Errorf("WithoutCancel lost the id: %q ok=%v", got, ok)
	}
}

// [F37-08] Attr：有代號時 key 為 req；沒有時 JSON 行沒有 req 鍵（不是空字串）.
func TestAttrPresentAndAbsent(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	log.Info("with", Attr(With(context.Background())))
	log.Info("without", Attr(context.Background()))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %s", len(lines), buf.String())
	}
	m := regexp.MustCompile(`"req":"([0-9a-f]{16})"`).FindStringSubmatch(lines[0])
	if m == nil {
		t.Errorf("line with id lacks a 16-hex req: %s", lines[0])
	}
	if strings.Contains(lines[1], `"req"`) {
		t.Errorf("line without id must not have a req key: %s", lines[1])
	}
}
