package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminTokenFromRequestHeader(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Header.Set(adminTokenHeader, " secret ")

	if got := adminTokenFromRequest(req); got != "secret" {
		t.Fatalf("adminTokenFromRequest() = %q, want secret", got)
	}
}

func TestAdminTokenFromRequestBearer(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Header.Set("Authorization", "Bearer secret")

	if got := adminTokenFromRequest(req); got != "secret" {
		t.Fatalf("adminTokenFromRequest() = %q, want secret", got)
	}
}
