package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
	"testing"
)

// [F33-04] providerDown 只認供應商端故障：逾時、連不上、5xx.
func TestProviderDownClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped deadline exceeded", fmt.Errorf("call: %w", context.DeadlineExceeded), true},
		{"net op error", &net.OpError{Op: "dial", Err: io.ErrClosedPipe}, true},
		{"dns error", &net.DNSError{Err: "no such host", Name: "x"}, true},
		{"url error connection refused", &url.Error{Op: "Post", URL: "http://x", Err: syscall.ECONNREFUSED}, true},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"500", &APIError{Status: 500}, true},
		{"502", &APIError{Status: 502}, true},
		{"503", &APIError{Status: 503}, true},
		{"504", &APIError{Status: 504}, true},
		{"599", &APIError{Status: 599}, true},
		{"wrapped 500", fmt.Errorf("x: %w", &APIError{Status: 500}), true},

		{"400", &APIError{Status: 400}, false},
		{"403", &APIError{Status: 403}, false},
		{"404", &APIError{Status: 404}, false},
		{"429", &APIError{Status: 429}, false},
		{"499", &APIError{Status: 499}, false},
		{"empty response", ErrEmpty, false},
		{"rejected", ErrRejected, false},
		{"plain error", fmt.Errorf("overloaded"), false},
		{"json syntax error", &json.SyntaxError{}, false},
		{"canceled", context.Canceled, false},
		{"url error wrapping canceled", &url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerDown(tc.err); got != tc.want {
				t.Errorf("providerDown(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
