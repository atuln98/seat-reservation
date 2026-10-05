package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"seat-reservation/internal/user"
)

type stubUserOperations struct {
	login func(context.Context, string, string) (user.Authentication, error)
}

func (stub stubUserOperations) Register(context.Context, string, string) (user.Authentication, error) {
	panic("unexpected Register call")
}

func (stub stubUserOperations) Login(ctx context.Context, email string, password string) (user.Authentication, error) {
	return stub.login(ctx, email, password)
}

func (stub stubUserOperations) Get(context.Context, string) (user.User, error) {
	panic("unexpected Get call")
}

func TestCredentialsRequestValidate(t *testing.T) {
	tests := []struct {
		name        string
		credentials credentialsRequest
		want        validationErrors
	}{
		{
			name:        "valid",
			credentials: credentialsRequest{Email: "buyer@example.com", Password: "password123"},
			want:        validationErrors{},
		},
		{
			name:        "invalid email",
			credentials: credentialsRequest{Email: "invalid", Password: "password123"},
			want:        validationErrors{"email": "must be a valid email address"},
		},
		{
			name:        "missing fields",
			credentials: credentialsRequest{},
			want: validationErrors{
				"email":    "must be a valid email address",
				"password": "must contain between 8 and 72 bytes",
			},
		},
		{
			name:        "short password",
			credentials: credentialsRequest{Email: "buyer@example.com", Password: "1234567"},
			want:        validationErrors{"password": "must contain between 8 and 72 bytes"},
		},
		{
			name:        "maximum password",
			credentials: credentialsRequest{Email: "buyer@example.com", Password: strings.Repeat("a", 72)},
			want:        validationErrors{},
		},
		{
			name:        "password over byte limit",
			credentials: credentialsRequest{Email: "buyer@example.com", Password: strings.Repeat("é", 37)},
			want:        validationErrors{"password": "must contain between 8 and 72 bytes"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.credentials.validate(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("validate() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestReadCredentials(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantError   string
	}{
		{
			name:       "missing content type",
			body:       `{"email":"buyer@example.com","password":"password123"}`,
			wantStatus: http.StatusUnsupportedMediaType,
			wantError:  "content_type_must_be_application_json",
		},
		{
			name:        "malformed JSON",
			contentType: "application/json",
			body:        `{"email":`,
			wantStatus:  http.StatusBadRequest,
			wantError:   "invalid_request",
		},
		{
			name:        "unknown field",
			contentType: "application/json",
			body:        `{"email":"buyer@example.com","password":"password123","admin":true}`,
			wantStatus:  http.StatusBadRequest,
			wantError:   "invalid_request",
		},
		{
			name:        "multiple objects",
			contentType: "application/json",
			body:        `{"email":"buyer@example.com","password":"password123"} {}`,
			wantStatus:  http.StatusBadRequest,
			wantError:   "invalid_request",
		},
		{
			name:        "invalid fields",
			contentType: "application/json",
			body:        `{"email":"invalid","password":"short"}`,
			wantStatus:  http.StatusBadRequest,
			wantError:   "validation_failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()

			if _, ok := readCredentials(response, request); ok {
				t.Fatal("readCredentials() succeeded")
			}
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}

			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body["error"] != test.wantError {
				t.Fatalf("error = %v, want %q", body["error"], test.wantError)
			}
		})
	}
}

func TestReadCredentialsValid(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/auth/register",
		strings.NewReader(`{"email":"buyer@example.com","password":"password123"}`),
	)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response := httptest.NewRecorder()

	credentials, ok := readCredentials(response, request)
	if !ok {
		t.Fatalf("readCredentials() failed with status %d and body %s", response.Code, response.Body.String())
	}
	if credentials.Email != "buyer@example.com" || credentials.Password != "password123" {
		t.Fatalf("credentials = %#v", credentials)
	}
}

func TestLoginNonexistentEmail(t *testing.T) {
	api := &API{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		users: stubUserOperations{
			login: func(context.Context, string, string) (user.Authentication, error) {
				return user.Authentication{}, user.ErrInvalidCredentials
			},
		},
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/auth/login",
		strings.NewReader(`{"email":"missing@example.com","password":"password123"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	api.login(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(body, map[string]string{"error": "invalid_credentials"}) {
		t.Fatalf("body = %#v", body)
	}
}
