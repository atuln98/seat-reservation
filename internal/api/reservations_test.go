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
	"time"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/reservation"
)

type stubReservationOperations struct {
	reserve func(context.Context, reservation.ReserveInput) (reservation.ReserveResult, error)
	cancel  func(context.Context, string, string) (reservation.Reservation, error)
}

func (stub stubReservationOperations) Reserve(
	ctx context.Context,
	input reservation.ReserveInput,
) (reservation.ReserveResult, error) {
	if stub.reserve == nil {
		panic("unexpected Reserve call")
	}
	return stub.reserve(ctx, input)
}

func (stub stubReservationOperations) Cancel(
	ctx context.Context,
	reservationID string,
	userID string,
) (reservation.Reservation, error) {
	if stub.cancel == nil {
		panic("unexpected Cancel call")
	}
	return stub.cancel(ctx, reservationID, userID)
}

func TestReserveSeatsRequestValidation(t *testing.T) {
	tests := []struct {
		name    string
		request reserveSeatsRequest
		want    validationErrors
	}{
		{
			name: "valid",
			request: reserveSeatsRequest{
				Seats:          []string{"A1", "A2"},
				IdempotencyKey: "0a655609-1a61-4f13-ab7d-9914ea9639e4",
			},
			want: validationErrors{},
		},
		{
			name:    "missing fields",
			request: reserveSeatsRequest{},
			want: validationErrors{
				"seats":           "must contain at least one seat",
				"idempotency_key": "must contain between 1 and 128 bytes",
			},
		},
		{
			name: "duplicate normalized seats",
			request: reserveSeatsRequest{
				Seats:          []string{"A1", " A1 "},
				IdempotencyKey: "key",
			},
			want: validationErrors{"seats": "must contain unique seat numbers"},
		},
		{
			name: "key with surrounding whitespace",
			request: reserveSeatsRequest{
				Seats:          []string{"A1"},
				IdempotencyKey: " key ",
			},
			want: validationErrors{"idempotency_key": "must not start or end with whitespace"},
		},
		{
			name: "key over byte limit",
			request: reserveSeatsRequest{
				Seats:          []string{"A1"},
				IdempotencyKey: strings.Repeat("a", 129),
			},
			want: validationErrors{"idempotency_key": "must contain between 1 and 128 bytes"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.request.normalizeAndValidate(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalizeAndValidate() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestReadReserveSeatsRejectsSpoofedUser(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/shows/1a6d7e70-2220-4b39-a1d0-5fb24760fb43/reserve",
		strings.NewReader(`{"seats":["A1"],"idempotency_key":"key","user_id":"another-user"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	if _, ok := readReserveSeats(response, request); ok {
		t.Fatal("readReserveSeats() succeeded")
	}
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if response.Body.String() != "{\"error\":\"invalid_request\"}\n" {
		t.Fatalf("body = %q", response.Body.String())
	}
}

func TestReserveSeatsResponses(t *testing.T) {
	showID := "1a6d7e70-2220-4b39-a1d0-5fb24760fb43"
	userID := "56d707ea-c3ca-423d-840d-f2cbce35ee5d"
	expected := reservation.Reservation{
		ID:          "1115a367-4400-4fa9-a88e-55c0d2f254ac",
		ShowID:      showID,
		UserID:      userID,
		Seats:       []string{"A1"},
		AmountPaise: 25000,
		Status:      reservation.StatusConfirmed,
	}

	tests := []struct {
		name       string
		result     reservation.ReserveResult
		err        error
		wantStatus int
		wantError  string
	}{
		{
			name:       "created",
			result:     reservation.ReserveResult{Reservation: expected},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "idempotent replay",
			result:     reservation.ReserveResult{Reservation: expected, Replayed: true},
			wantStatus: http.StatusOK,
		},
		{
			name:       "show missing",
			err:        reservation.ErrShowNotFound,
			wantStatus: http.StatusNotFound,
			wantError:  "show_not_found",
		},
		{
			name:       "seat unavailable",
			err:        reservation.ErrSeatsUnavailable,
			wantStatus: http.StatusConflict,
			wantError:  "seats_unavailable",
		},
		{
			name:       "limit exceeded",
			err:        reservation.ErrUserLimitExceeded,
			wantStatus: http.StatusConflict,
			wantError:  "seat_limit_exceeded",
		},
		{
			name:       "idempotency conflict",
			err:        reservation.ErrIdempotencyConflict,
			wantStatus: http.StatusConflict,
			wantError:  "idempotency_conflict",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &API{
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				reservations: stubReservationOperations{
					reserve: func(_ context.Context, input reservation.ReserveInput) (reservation.ReserveResult, error) {
						if input.ShowID != showID || input.UserID != userID {
							t.Fatalf("input = %#v", input)
						}
						return test.result, test.err
					},
				},
			}
			request := httptest.NewRequest(
				http.MethodPost,
				"/shows/"+showID+"/reserve",
				strings.NewReader(`{"seats":["A1"],"idempotency_key":"key"}`),
			)
			request.SetPathValue("id", showID)
			request.Header.Set("Content-Type", "application/json")
			response := serveAuthenticated(t, userID, auth.RoleUser, http.HandlerFunc(api.reserveSeats), request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantError != "" {
				var body map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if body["error"] != test.wantError {
					t.Fatalf("error = %q, want %q", body["error"], test.wantError)
				}
			}
		})
	}
}

func TestCancelReservationResponses(t *testing.T) {
	reservationID := "1115a367-4400-4fa9-a88e-55c0d2f254ac"
	userID := "56d707ea-c3ca-423d-840d-f2cbce35ee5d"

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantError  string
	}{
		{
			name:       "cancelled",
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing",
			err:        reservation.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantError:  "reservation_not_found",
		},
		{
			name:       "not owner",
			err:        reservation.ErrNotOwner,
			wantStatus: http.StatusForbidden,
			wantError:  "reservation_not_owned",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &API{
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				reservations: stubReservationOperations{
					cancel: func(_ context.Context, gotReservationID string, gotUserID string) (reservation.Reservation, error) {
						if gotReservationID != reservationID || gotUserID != userID {
							t.Fatalf("reservation ID = %q, user ID = %q", gotReservationID, gotUserID)
						}
						return reservation.Reservation{ID: reservationID, UserID: userID, Status: reservation.StatusCancelled}, test.err
					},
				},
			}
			request := httptest.NewRequest(http.MethodPost, "/reservations/"+reservationID+"/cancel", nil)
			request.SetPathValue("id", reservationID)
			response := serveAuthenticated(t, userID, auth.RoleUser, http.HandlerFunc(api.cancelReservation), request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantError != "" {
				var body map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if body["error"] != test.wantError {
					t.Fatalf("error = %q, want %q", body["error"], test.wantError)
				}
			}
		})
	}
}

func serveAuthenticated(
	t *testing.T,
	userID string,
	role auth.Role,
	handler http.Handler,
	request *http.Request,
) *httptest.ResponseRecorder {
	t.Helper()
	manager, err := auth.NewTokenManager("test-jwt-secret-with-at-least-32-bytes", "seat-reservation", time.Hour)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	token, err := manager.Issue(userID, role)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	manager.Middleware(handler).ServeHTTP(response, request)
	return response
}
