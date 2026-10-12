// Package upstream classifies upstream HTTP errors into the coarse classes used
// by the pool health state machine, looking past the status code at the error
// body's machine-readable code/type/message.
package upstream

import (
	"encoding/json"
	"strconv"
	"strings"

	"lmgateway/internal/packet"
)

var quotaCodes = map[string]bool{
	"insufficient_quota":         true,
	"insufficient_balance":       true,
	"insufficient_credits":       true,
	"insufficient_funds":         true,
	"quota_exceeded":             true,
	"billing_hard_limit_reached": true,
	"payment_required":           true,
	"credit_balance_too_low":     true,
}

var quotaKeywords = []string{
	"insufficient quota",
	"insufficient balance",
	"insufficient credits",
	"insufficient funds",
	"credit balance is too low",
	"quota exceeded",
	"exceeded your current quota",
	"payment required",
	"out of credits",
	"no credits",
}

// ClassifyError returns the coarse class for an HTTP status + response body.
func ClassifyError(status int, body []byte) string {
	code, message := parseErrorFields(body)
	return ClassifyFields(status, code, message)
}

// ClassifyFields classifies from already-extracted fields (used for event-based
// errors that have no HTTP status).
func ClassifyFields(status int, code, message string) string {
	if isQuotaCode(code) || isQuotaText(message) || status == 402 {
		return packet.ClassQuota
	}
	return packet.ClassifyStatus(status)
}

func isQuotaCode(code string) bool {
	return quotaCodes[strings.ToLower(strings.TrimSpace(code))]
}

func isQuotaText(message string) bool {
	m := strings.ToLower(message)
	for _, kw := range quotaKeywords {
		if strings.Contains(m, kw) {
			return true
		}
	}
	return false
}

func parseErrorFields(body []byte) (code, message string) {
	if len(body) == 0 {
		return "", ""
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return "", ""
	}
	if e, ok := obj["error"].(map[string]any); ok {
		code = pickString(e["code"], e["type"])
		message = asString(e["message"])
	}
	if code == "" {
		code = pickString(obj["code"], obj["type"])
	}
	if message == "" {
		message = asString(obj["message"])
	}
	return code, message
}

func pickString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
		if f, ok := v.(float64); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	return ""
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
