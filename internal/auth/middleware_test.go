package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMiddlewareRejectsAuthenticationFailures(t *testing.T) {
	manager, err := NewTokenManager("test-jwt-secret-with-at-least-32-bytes", "seat-reservation", time.Hour)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}

	token, err := manager.Issue("a8ba0dc2-bfd8-46a6-8419-05ca8e99251c", RoleUser)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	replacement := "A"
	if token[len(token)-1:] == replacement {
		replacement = "B"
	}

	tests := []struct {
		name       string
		header     string
		wantError  string
		wantStatus int
	}{
		{
			name:       "missing header",
			wantError:  "authentication_required",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong scheme",
			header:     "Basic credentials",
			wantError:  "authentication_required",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing bearer token",
			header:     "Bearer ",
			wantError:  "authentication_required",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "malformed token",
			header:     "Bearer invalid",
			wantError:  "invalid_token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "tampered signature",
			header:     "Bearer " + token[:len(token)-1] + replacement,
			wantError:  "invalid_token",
			wantStatus: http.StatusUnauthorized,
		},
	}

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called")
	})
	handler := manager.Middleware(next)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/users/me", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body["error"] != test.wantError {
				t.Fatalf("error = %q, want %q", body["error"], test.wantError)
			}
		})
	}
}

func TestMiddlewareAddsPrincipal(t *testing.T) {
	manager, err := NewTokenManager("test-jwt-secret-with-at-least-32-bytes", "seat-reservation", time.Hour)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}

	userID := "a8ba0dc2-bfd8-46a6-8419-05ca8e99251c"
	token, err := manager.Issue(userID, RoleAdmin)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, ok := PrincipalFromContext(request.Context())
		if !ok {
			t.Fatal("principal missing")
		}
		if principal.UserID != userID || principal.Role != RoleAdmin {
			t.Fatalf("principal = %#v", principal)
		}
		writer.WriteHeader(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/users/me", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()

	manager.Middleware(next).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}
