package api

import (
	"errors"
	"net/http"
	"strings"

	"seat-reservation/internal/auth"
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
			writeError(writer, http.StatusUnsupportedMediaType, "content_type_must_be_application_json")
		} else {
			writeError(writer, http.StatusBadRequest, "invalid_request")
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
		writeError(writer, http.StatusBadRequest, "invalid_show_id")
		return
	}

	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication_required")
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
			writeError(writer, http.StatusNotFound, "show_not_found")
		case errors.Is(err, reservation.ErrSeatsUnavailable):
			api.metrics.Declined("seat_taken")
			writeError(writer, http.StatusConflict, "seats_unavailable")
		case errors.Is(err, reservation.ErrUserLimitExceeded):
			api.metrics.Declined("per_user_limit")
			writeError(writer, http.StatusConflict, "seat_limit_exceeded")
		case errors.Is(err, reservation.ErrIdempotencyConflict):
			api.metrics.Declined("idempotency_conflict")
			writeError(writer, http.StatusConflict, "idempotency_conflict")
		case errors.Is(err, reservation.ErrAmountOutOfRange):
			writeError(writer, http.StatusUnprocessableEntity, "amount_out_of_range")
		default:
			requestLogger(api.logger, request).Error(
				"seat reservation failed",
				"event", "reservation_failed",
				"showId", showID,
				"userId", principal.UserID,
				"error", err,
			)
			writeError(writer, http.StatusInternalServerError, "internal_error")
		}
		return
	}

	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		api.metrics.Replayed()
	} else {
		api.metrics.Confirmed()
	}
	event := "reservation_confirmed"
	message := "reservation confirmed"
	if result.Replayed {
		event = "reservation_replayed"
		message = "reservation replayed"
	}
	requestLogger(api.logger, request).Info(
		message,
		"event", event,
		"reservationId", result.Reservation.ID,
		"showId", result.Reservation.ShowID,
		"userId", result.Reservation.UserID,
		"seatCount", len(result.Reservation.Seats),
		"amountPaise", result.Reservation.AmountPaise,
	)
	writeJSON(writer, status, result.Reservation)
}

func (api *API) cancelReservation(writer http.ResponseWriter, request *http.Request) {
	reservationID, ok := canonicalUUID(request.PathValue("id"))
	if !ok {
		writeError(writer, http.StatusBadRequest, "invalid_reservation_id")
		return
	}

	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication_required")
		return
	}

	cancelled, err := api.reservations.Cancel(request.Context(), reservationID, principal.UserID)
	if err != nil {
		if writeContextError(writer, err) {
			return
		}
		switch {
		case errors.Is(err, reservation.ErrNotFound):
			writeError(writer, http.StatusNotFound, "reservation_not_found")
		case errors.Is(err, reservation.ErrInvalidState):
			writeError(writer, http.StatusConflict, "reservation_state_conflict")
		default:
			requestLogger(api.logger, request).Error(
				"reservation cancellation failed",
				"event", "reservation_cancellation_failed",
				"reservationId", reservationID,
				"userId", principal.UserID,
				"error", err,
			)
			writeError(writer, http.StatusInternalServerError, "internal_error")
		}
		return
	}

	requestLogger(api.logger, request).Info(
		"reservation cancelled",
		"event", "reservation_cancelled",
		"reservationId", cancelled.ID,
		"showId", cancelled.ShowID,
		"userId", cancelled.UserID,
		"seatCount", len(cancelled.Seats),
	)
	writeJSON(writer, http.StatusOK, cancelled)
}
