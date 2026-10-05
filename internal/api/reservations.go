package api

import (
	"errors"
	"net/http"
	"strings"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/httpmiddleware"
	"seat-reservation/internal/reservation"
)

type reserveSeatsRequest struct {
	Seats          []string `json:"seats"`
	IdempotencyKey string   `json:"idempotency_key"`
}

func (request *reserveSeatsRequest) normalizeAndValidate() validationErrors {
	fields := validationErrors{}
	normalizedSeats, seatError := normalizeSeatNumbers(request.Seats)
	request.Seats = normalizedSeats
	if seatError != "" {
		fields["seats"] = seatError
	}

	keyLength := len([]byte(request.IdempotencyKey))
	if keyLength == 0 || keyLength > 128 {
		fields["idempotency_key"] = "must contain between 1 and 128 bytes"
	} else if strings.TrimSpace(request.IdempotencyKey) != request.IdempotencyKey {
		fields["idempotency_key"] = "must not start or end with whitespace"
	} else if containsControlCharacter(request.IdempotencyKey) {
		fields["idempotency_key"] = "must not contain control characters"
	}
	return fields
}

func readReserveSeats(writer http.ResponseWriter, request *http.Request) (reserveSeatsRequest, bool) {
	var body reserveSeatsRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			writeJSON(writer, http.StatusUnsupportedMediaType, map[string]string{"error": "content_type_must_be_application_json"})
		} else {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		}
		return reserveSeatsRequest{}, false
	}

	fields := body.normalizeAndValidate()
	if len(fields) != 0 {
		writeJSON(writer, http.StatusBadRequest, validationErrorResponse{
			Error:  "validation_failed",
			Fields: fields,
		})
		return reserveSeatsRequest{}, false
	}
	return body, true
}

func (api *API) reserveSeats(writer http.ResponseWriter, request *http.Request) {
	showID, ok := canonicalUUID(request.PathValue("id"))
	if !ok {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_show_id"})
		return
	}

	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}

	body, ok := readReserveSeats(writer, request)
	if !ok {
		return
	}

	result, err := api.reservations.Reserve(request.Context(), reservation.ReserveInput{
		ShowID:         showID,
		UserID:         principal.UserID,
		SeatNumbers:    body.Seats,
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		if writeContextError(writer, err) {
			return
		}
		switch {
		case errors.Is(err, reservation.ErrShowNotFound):
			writeJSON(writer, http.StatusNotFound, map[string]string{"error": "show_not_found"})
		case errors.Is(err, reservation.ErrSeatsUnavailable):
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "seats_unavailable"})
		case errors.Is(err, reservation.ErrUserLimitExceeded):
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "seat_limit_exceeded"})
		case errors.Is(err, reservation.ErrIdempotencyConflict):
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "idempotency_conflict"})
		case errors.Is(err, reservation.ErrAmountOutOfRange):
			writeJSON(writer, http.StatusUnprocessableEntity, map[string]string{"error": "amount_out_of_range"})
		default:
			api.logger.Error(
				"seat reservation failed",
				"request_id", httpmiddleware.RequestIDFromContext(request.Context()),
				"show_id", showID,
				"user_id", principal.UserID,
				"error", err,
			)
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		}
		return
	}

	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(writer, status, result.Reservation)
}

func (api *API) cancelReservation(writer http.ResponseWriter, request *http.Request) {
	reservationID, ok := canonicalUUID(request.PathValue("id"))
	if !ok {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_reservation_id"})
		return
	}

	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}

	cancelled, err := api.reservations.Cancel(request.Context(), reservationID, principal.UserID)
	if err != nil {
		if writeContextError(writer, err) {
			return
		}
		switch {
		case errors.Is(err, reservation.ErrNotFound):
			writeJSON(writer, http.StatusNotFound, map[string]string{"error": "reservation_not_found"})
		case errors.Is(err, reservation.ErrInvalidState):
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "reservation_state_conflict"})
		default:
			api.logger.Error(
				"reservation cancellation failed",
				"request_id", httpmiddleware.RequestIDFromContext(request.Context()),
				"reservation_id", reservationID,
				"user_id", principal.UserID,
				"error", err,
			)
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		}
		return
	}

	writeJSON(writer, http.StatusOK, cancelled)
}
