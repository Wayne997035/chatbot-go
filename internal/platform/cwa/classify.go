package cwa

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// 分類詞彙（固定集合；status_<code> 由 Client 直接指定）.
const (
	classDNS       = "dns"
	classTLS       = "tls"
	classTimeout   = "timeout"
	classConnReset = "conn_reset"
	classNetwork   = "network"
	classDecode    = "decode"
	classTooLarge  = "too_large"
	classQuota     = "quota_reserved"
)

// Classify 把網路層錯誤分成固定詞彙（F30-32，規格 T7(a)，依序判）：
// dns、tls、timeout、conn_reset，其餘為 network. 結果不含任何原訊息.
func Classify(err error) string {
	if err == nil {
		return classNetwork
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return classDNS
	}
	if isTLSError(err) {
		return classTLS
	}
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return classTimeout
	}
	if errors.Is(err, syscall.ECONNRESET) || strings.Contains(err.Error(), "connection reset") {
		return classConnReset
	}
	return classNetwork
}

func isTLSError(err error) bool {
	var (
		hostErr x509.HostnameError
		authErr x509.UnknownAuthorityError
		certErr x509.CertificateInvalidError
		recErr  tls.RecordHeaderError
		verErr  *tls.CertificateVerificationError
	)
	if errors.As(err, &hostErr) || errors.As(err, &authErr) || errors.As(err, &certErr) ||
		errors.As(err, &recErr) || errors.As(err, &verErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "TLS handshake timeout") || strings.Contains(msg, "x509:") ||
		strings.HasPrefix(msg, "tls:")
}

// ClassOf 取得錯誤的分類（F30-32）：*Error 取其 Class，裸 ErrQuotaReserved 回 quota_reserved，其他一律 network.
// 下游 log 點只准用它取 class 欄位.
func ClassOf(err error) string {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Class
	}
	if errors.Is(err, ErrQuotaReserved) {
		return classQuota
	}
	return classNetwork
}

// transient 判斷網路層錯誤是否值得重試（規格 T2）：net.Error、連線被重置、EOF、單次嘗試期限到.
func transient(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "TLS handshake timeout")
}
