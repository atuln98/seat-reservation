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
		request.RemoteAddr = "203.0.113.1:1234"
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
	request.RemoteAddr = "203.0.113.2:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("other client status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestClientIP(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "127.0.0.1:1234"

	if got := clientIP(request); got != "127.0.0.1" {
		t.Fatalf("clientIP() = %q", got)
	}
}
