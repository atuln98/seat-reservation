package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteContextError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "cancelled",
			err:        fmt.Errorf("database operation: %w", context.Canceled),
			wantStatus: statusClientClosedRequest,
			wantCode:   "request_cancelled",
		},
		{
			name:       "deadline",
			err:        fmt.Errorf("database operation: %w", context.DeadlineExceeded),
			wantStatus: http.StatusRequestTimeout,
			wantCode:   "request_timeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			if !writeContextError(response, test.err) {
				t.Fatal("writeContextError() = false")
			}
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body["error"] != test.wantCode {
				t.Fatalf("error = %q, want %q", body["error"], test.wantCode)
			}
		})
	}
}

func TestWriteContextErrorIgnoresOtherErrors(t *testing.T) {
	response := httptest.NewRecorder()
	if writeContextError(response, errors.New("other")) {
		t.Fatal("writeContextError() = true")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
}
