package api

import (
	"errors"
	"math"
	"net/http"
	"strings"

	"seat-reservation/internal/show"
)

type createShowRequest struct {
	Name         string   `json:"name"`
	Seats        []string `json:"seats"`
	PricePaise   *int64   `json:"price_paise"`
	PerUserLimit *int     `json:"per_user_limit"`
}

const maxPostgresInteger = 1<<31 - 1

func (request *createShowRequest) normalizeAndValidate() validationErrors {
	request.Name = strings.TrimSpace(request.Name)

	fields := validationErrors{}
	if length := len([]byte(request.Name)); length == 0 || length > 200 {
		fields["name"] = "must contain between 1 and 200 bytes"
	} else if containsControlCharacter(request.Name) {
		fields["name"] = "must not contain control characters"
	}
	if request.PricePaise == nil {
		fields["price_paise"] = "is required"
	} else if *request.PricePaise < 0 {
		fields["price_paise"] = "must be greater than or equal to zero"
	}
	if request.PerUserLimit != nil && (*request.PerUserLimit <= 0 || *request.PerUserLimit > maxPostgresInteger) {
		fields["per_user_limit"] = "must contain a value between 1 and 2147483647"
	}
	normalizedSeats, seatError := normalizeSeatNumbers(request.Seats)
	request.Seats = normalizedSeats
	if seatError != "" {
		fields["seats"] = seatError
	}

	maximumReservationSeats := len(request.Seats)
	if maximumReservationSeats > show.DefaultPerUserLimit {
		maximumReservationSeats = show.DefaultPerUserLimit
	}
	if request.PerUserLimit != nil && *request.PerUserLimit > 0 && *request.PerUserLimit < maximumReservationSeats {
		maximumReservationSeats = *request.PerUserLimit
	}
	if request.PricePaise != nil &&
		*request.PricePaise >= 0 &&
		maximumReservationSeats > 0 &&
		*request.PricePaise > math.MaxInt64/int64(maximumReservationSeats) {
		fields["price_paise"] = "is too large for the maximum reservation size"
	}
	return fields
}

func readCreateShow(writer http.ResponseWriter, request *http.Request) (show.CreateInput, bool) {
	var body createShowRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			writeJSON(writer, http.StatusUnsupportedMediaType, map[string]string{"error": "content_type_must_be_application_json"})
		} else {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		}
		return show.CreateInput{}, false
	}

	fields := body.normalizeAndValidate()
	if len(fields) != 0 {
		writeJSON(writer, http.StatusBadRequest, validationErrorResponse{
			Error:  "validation_failed",
			Fields: fields,
		})
		return show.CreateInput{}, false
	}

	perUserLimit := show.DefaultPerUserLimit
	if body.PerUserLimit != nil {
		perUserLimit = *body.PerUserLimit
	}

	return show.CreateInput{
		Name:         body.Name,
		SeatNumbers:  body.Seats,
		PricePaise:   *body.PricePaise,
		PerUserLimit: perUserLimit,
	}, true
}

func (api *API) createShow(writer http.ResponseWriter, request *http.Request) {
	input, ok := readCreateShow(writer, request)
	if !ok {
		return
	}

	created, err := api.shows.Create(request.Context(), input)
	if err != nil {
		if writeContextError(writer, err) {
			return
		}
		requestLogger(api.logger, request).Error(
			"show creation failed",
			"event", "show_creation_failed",
			"error", err,
		)
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	requestLogger(api.logger, request).Info(
		"show created",
		"event", "show_created",
		"showId", created.ID,
		"seatCount", len(created.Seats),
		"pricePaise", created.PricePaise,
		"perUserLimit", created.PerUserLimit,
	)
	writeJSON(writer, http.StatusCreated, created)
}

func (api *API) getShow(writer http.ResponseWriter, request *http.Request) {
	showID, ok := canonicalUUID(request.PathValue("id"))
	if !ok {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_show_id"})
		return
	}

	existing, err := api.shows.Get(request.Context(), showID)
	if err != nil {
		if writeContextError(writer, err) {
			return
		}
		if errors.Is(err, show.ErrNotFound) {
			writeJSON(writer, http.StatusNotFound, map[string]string{"error": "show_not_found"})
			return
		}
		requestLogger(api.logger, request).Error(
			"show lookup failed",
			"event", "show_lookup_failed",
			"showId", showID,
			"error", err,
		)
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	writeJSON(writer, http.StatusOK, existing)
}
