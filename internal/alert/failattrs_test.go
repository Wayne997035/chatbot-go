package alert

import (
	"chatbot-go/internal/platform/cwa"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestFailAttrsListsEveryJoinedClass [F302-02] 一輪多個抓取失敗（含 Join 套 Join）時，log 欄位列出每個分類、非氣象署錯誤另列遮蔽字串，且不含任何內層原文.
func TestFailAttrsListsEveryJoinedClass(t *testing.T) {
	newErr := func(class string, n int) *cwa.Error {
		return &cwa.Error{Class: class, Attempts: 1, Err: fmt.Errorf("LEAK-%d https://host.example/%s", n, class)}
	}
	errA, errB, errC := newErr("dns", 1), newErr("tls", 2), newErr("timeout", 3)
	errA2 := newErr("dns", 4)

	cases := []struct {
		name         string
		err          error
		wantClass    string // 空字串 = 不得有 class 欄
		wantErrorSub string // 空字串 = 不得有 error 欄
	}{
		{"a flat join", errors.Join(errA, errB), "dns,tls", ""},
		{
			"b nested join",
			errors.Join(fmt.Errorf("w: %w", errA), errors.Join(fmt.Errorf("e: %w", errB), fmt.Errorf("t: %w", errC))),
			"dns,tls,timeout", "",
		},
		{"c mixed join", errors.Join(errA, errors.New("mongo down")), "dns", "mongo down"},
		{"d duplicate class listed once", errors.Join(errA, errA2), "dns", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attrs := failAttrs(tc.err)
			if len(attrs)%2 != 0 {
				t.Fatalf("attrs must be key/value pairs: %v", attrs)
			}
			got := map[string]string{}
			var all strings.Builder
			for i := 0; i < len(attrs); i += 2 {
				k, _ := attrs[i].(string)
				v, _ := attrs[i+1].(string)
				got[k] = v
				all.WriteString(v)
			}
			if got["class"] != tc.wantClass {
				t.Errorf("class = %q, want %q (attrs %v)", got["class"], tc.wantClass, attrs)
			}
			_, hasErr := got["error"]
			if tc.wantErrorSub == "" && hasErr {
				t.Errorf("unexpected error attr: %v", attrs)
			}
			if tc.wantErrorSub != "" && !strings.Contains(got["error"], tc.wantErrorSub) {
				t.Errorf("error = %q, want it to contain %q", got["error"], tc.wantErrorSub)
			}
			if strings.Contains(all.String(), "LEAK-") {
				t.Errorf("log values leak inner text: %v", attrs)
			}
		})
	}
}
