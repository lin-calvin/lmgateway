package upstream

import (
	"testing"

	"lmgateway/internal/packet"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"openai insufficient_quota", 429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, packet.ClassQuota},
		{"deepseek 402 balance", 402, `{"error":{"message":"Insufficient Balance","type":"unknown_error","code":"invalid_request_error"}}`, packet.ClassQuota},
		{"openrouter 402", 402, `{"error":{"message":"Insufficient credits","code":402}}`, packet.ClassQuota},
		{"balance message 400", 400, `{"error":{"message":"Your credit balance is too low"}}`, packet.ClassQuota},
		{"plain rate limit", 429, `{"error":{"message":"Rate limit reached for requests","type":"rate_limit_exceeded"}}`, packet.ClassRateLimit},
		{"overloaded", 503, `{"error":{"message":"overloaded"}}`, packet.ClassOverloaded},
		{"auth", 401, `{"error":{"message":"invalid api key"}}`, packet.ClassAuth},
		{"server error", 500, `{"error":{"message":"boom"}}`, packet.ClassUpstream},
		{"no body 429", 429, ``, packet.ClassRateLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("ClassifyError(%d, %s) = %q, want %q", tc.status, tc.body, got, tc.want)
			}
		})
	}
}
