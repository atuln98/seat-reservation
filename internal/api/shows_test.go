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

	"seat-reservation/internal/show"
)

type stubShowOperations struct {
	create func(context.Context, show.CreateInput) (show.Show, error)
	get    func(context.Context, string) (show.Show, error)
}

func (stub stubShowOperations) Create(ctx context.Context, input show.CreateInput) (show.Show, error) {
	if stub.create == nil {
		panic("unexpected Create call")
	}
	return stub.create(ctx, input)
}

func (stub stubShowOperations) Get(ctx context.Context, showID string) (show.Show, error) {
	if stub.get == nil {
		panic("unexpected Get call")
	}
	return stub.get(ctx, showID)
}

func TestCreateShowRequestValidation(t *testing.T) {
	zero := 0
	overflow := maxPostgresInteger + 1
	negativePrice := int64(-1)
	validPrice := int64(25000)

	tests := []struct {
		name    string
		request createShowRequest
		want    validationErrors
	}{
		{
			name: "valid",
			request: createShowRequest{
				Name:       "friday-night",
				Seats:      []string{"A1", "A2"},
				PricePaise: &validPrice,
			},
			want: validationErrors{},
		},
		{
			name:    "required fields",
			request: createShowRequest{},
			want: validationErrors{
				"name":        "must contain between 1 and 200 bytes",
				"price_paise": "is required",
				"seats":       "must contain at least one seat",
			},
		},
		{
			name: "duplicate normalized seats",
			request: createShowRequest{
				Name:       "show",
				Seats:      []string{"A1", " A1 "},
				PricePaise: &validPrice,
			},
			want: validationErrors{"seats": "must contain unique seat numbers"},
		},
		{
			name: "empty seat",
			request: createShowRequest{
				Name:       "show",
				Seats:      []string{"A1", " "},
				PricePaise: &validPrice,
			},
			want: validationErrors{"seats": "each seat number must contain between 1 and 32 bytes"},
		},
		{
			name: "negative price",
			request: createShowRequest{
				Name:       "show",
				Seats:      []string{"A1"},
				PricePaise: &negativePrice,
			},
			want: validationErrors{"price_paise": "must be greater than or equal to zero"},
		},
		{
			name: "invalid user limit",
			request: createShowRequest{
				Name:         "show",
				Seats:        []string{"A1"},
				PricePaise:   &validPrice,
				PerUserLimit: &zero,
			},
			want: validationErrors{"per_user_limit": "must contain a value between 1 and 2147483647"},
		},
		{
			name: "user limit over database range",
			request: createShowRequest{
				Name:         "show",
				Seats:        []string{"A1"},
				PricePaise:   &validPrice,
				PerUserLimit: &overflow,
			},
			want: validationErrors{"per_user_limit": "must contain a value between 1 and 2147483647"},
		},
		{
			name: "long name",
			request: createShowRequest{
				Name:       strings.Repeat("a", 201),
				Seats:      []string{"A1"},
				PricePaise: &validPrice,
			},
			want: validationErrors{"name": "must contain between 1 and 200 bytes"},
		},
		{
			name: "long seat number",
			request: createShowRequest{
				Name:       "show",
				Seats:      []string{strings.Repeat("a", 33)},
				PricePaise: &validPrice,
			},
			want: validationErrors{"seats": "each seat number must contain between 1 and 32 bytes"},
		},
		{
			name: "control character in name",
			request: createShowRequest{
				Name:       "show\x00name",
				Seats:      []string{"A1"},
				PricePaise: &validPrice,
			},
			want: validationErrors{"name": "must not contain control characters"},
		},
		{
			name: "control character in seat number",
			request: createShowRequest{
				Name:       "show",
				Seats:      []string{"A\n1"},
				PricePaise: &validPrice,
			},
			want: validationErrors{"seats": "seat numbers must not contain control characters"},
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

func TestReadCreateShowUsesDefaultsAndNormalizes(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/shows",
		strings.NewReader(`{"name":" friday-night ","seats":[" A1 ","A2"],"price_paise":25000}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	input, ok := readCreateShow(response, request)
	if !ok {
		t.Fatalf("readCreateShow() failed with status %d and body %s", response.Code, response.Body.String())
	}
	want := show.CreateInput{
		Name:         "friday-night",
		SeatNumbers:  []string{"A1", "A2"},
		PricePaise:   25000,
		PerUserLimit: show.DefaultPerUserLimit,
	}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("input = %#v, want %#v", input, want)
	}
}

func TestCreateShow(t *testing.T) {
	expected := show.Show{
		ID:           "1a6d7e70-2220-4b39-a1d0-5fb24760fb43",
		Name:         "friday-night",
		PricePaise:   25000,
		PerUserLimit: 4,
		Seats: []show.Seat{
			{Number: "A1", State: show.SeatAvailable},
			{Number: "A2", State: show.SeatAvailable},
		},
		Counts: show.Counts{Available: 2, Total: 2},
	}
	api := &API{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		shows: stubShowOperations{
			create: func(_ context.Context, input show.CreateInput) (show.Show, error) {
				if input.Name != expected.Name || !reflect.DeepEqual(input.SeatNumbers, []string{"A1", "A2"}) {
					t.Fatalf("input = %#v", input)
				}
				return expected, nil
			},
		},
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/shows",
		strings.NewReader(`{"name":"friday-night","seats":["A1","A2"],"price_paise":25000}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	api.createShow(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusCreated)
	}
	var body show.Show
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(body, expected) {
		t.Fatalf("body = %#v, want %#v", body, expected)
	}
}

func TestGetShowNotFound(t *testing.T) {
	showID := "1a6d7e70-2220-4b39-a1d0-5fb24760fb43"
	api := &API{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		shows: stubShowOperations{
			get: func(_ context.Context, gotID string) (show.Show, error) {
				if gotID != showID {
					t.Fatalf("show ID = %q, want %q", gotID, showID)
				}
				return show.Show{}, show.ErrNotFound
			},
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/shows/"+showID, nil)
	request.SetPathValue("id", showID)
	response := httptest.NewRecorder()

	api.getShow(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if response.Body.String() != "{\"error\":\"show_not_found\"}\n" {
		t.Fatalf("body = %q", response.Body.String())
	}
}

func TestGetShowRejectsInvalidID(t *testing.T) {
	api := &API{}
	request := httptest.NewRequest(http.MethodGet, "/shows/not-a-uuid", nil)
	request.SetPathValue("id", "not-a-uuid")
	response := httptest.NewRecorder()

	api.getShow(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if response.Body.String() != "{\"error\":\"invalid_show_id\"}\n" {
		t.Fatalf("body = %q", response.Body.String())
	}
}
