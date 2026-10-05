package httpmiddleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiterLimitsEachClient(t *testing.T) {
	limiter := NewRateLimiter(1, 2)
	handler := limiter.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))

	for attempt := 1; attempt <= 3; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		request.RemoteAddr = "100.64.0.1:1234"
		request.Header.Set("X-Real-IP", "203.0.113.1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		want := http.StatusNoContent
		if attempt == 3 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.Code, want)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	request.RemoteAddr = "100.64.0.1:1234"
	request.Header.Set("X-Real-IP", "203.0.113.2")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("other client status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		realIPs    []string
		want       string
	}{
		{
			name:       "Railway IPv4",
			remoteAddr: "100.64.0.1:1234",
			realIPs:    []string{"203.0.113.10"},
			want:       "203.0.113.10",
		},
		{
			name:       "Railway IPv6",
			remoteAddr: "100.64.0.1:1234",
			realIPs:    []string{"2001:db8::10"},
			want:       "2001:db8::10",
		},
		{
			name:       "direct connection",
			remoteAddr: "127.0.0.1:1234",
			want:       "127.0.0.1",
		},
		{
			name:       "direct IPv6 connection",
			remoteAddr: "[2001:db8::20]:1234",
			want:       "2001:db8::20",
		},
		{
			name:       "malformed forwarded address",
			remoteAddr: "192.0.2.1:1234",
			realIPs:    []string{"203.0.113.10:4321"},
			want:       "192.0.2.1",
		},
		{
			name:       "forwarded address list",
			remoteAddr: "192.0.2.1:1234",
			realIPs:    []string{"198.51.100.1, 203.0.113.10"},
			want:       "192.0.2.1",
		},
		{
			name:       "multiple forwarded headers",
			remoteAddr: "192.0.2.1:1234",
			realIPs:    []string{"198.51.100.1", "203.0.113.10"},
			want:       "192.0.2.1",
		},
		{
			name:       "malformed direct address",
			remoteAddr: "not-an-address",
			want:       "unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = test.remoteAddr
			for _, value := range test.realIPs {
				request.Header.Add("X-Real-IP", value)
			}
			if got := clientIP(request); got != test.want {
				t.Fatalf("clientIP() = %q, want %q", got, test.want)
			}
		})
	}
}
