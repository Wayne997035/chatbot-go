package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestValidateHMAC(t *testing.T) {
	secret := "test-secret-key"
	body := []byte(`{"events":[]}`)

	// 計算正確簽名
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name      string
		body      []byte
		signature string
		secret    string
		want      bool
	}{
		{"valid signature", body, validSig, secret, true},
		{"wrong signature", body, "wrong-signature", secret, false},
		{"wrong secret", body, validSig, "wrong-secret", false},
		{"empty body", []byte{}, validSig, secret, false},
		{"empty signature", body, "", secret, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateHMAC(tt.body, tt.signature, tt.secret)
			if got != tt.want {
				t.Errorf("validateHMAC() = %v, want %v", got, tt.want)
			}
		})
	}
}
