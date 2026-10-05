package reservation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"seat-reservation/internal/telemetry"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

var (
	ErrShowNotFound        = errors.New("show not found")
	ErrSeatsUnavailable    = errors.New("one or more seats are unavailable")
	ErrUserLimitExceeded   = errors.New("per-user seat limit exceeded")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different request")
	ErrAmountOutOfRange    = errors.New("reservation amount is out of range")
	ErrNotFound            = errors.New("reservation not found")
	ErrInvalidState        = errors.New("reservation state is invalid")
)

type Status string

const (
	StatusConfirmed Status = "confirmed"
	StatusCancelled Status = "cancelled"
)

type Reservation struct {
	ID          string   `json:"reservation_id"`
	ShowID      string   `json:"show_id"`
	UserID      string   `json:"user_id"`
	Seats       []string `json:"seats"`
	AmountPaise int64    `json:"amount_paise"`
	Status      Status   `json:"status"`
}

type ReserveInput struct {
	ShowID         string
	UserID         string
	SeatNumbers    []string
	IdempotencyKey string
}

type ReserveResult struct {
	Reservation Reservation
	Replayed    bool
}

type Service struct {
	database *pgxpool.Pool
}

func NewService(database *pgxpool.Pool) *Service {
	return &Service{database: database}
}

var reserveDeclines = []error{
	ErrShowNotFound,
	ErrSeatsUnavailable,
	ErrUserLimitExceeded,
	ErrIdempotencyConflict,
	ErrAmountOutOfRange,
}

var cancelDeclines = []error{ErrNotFound, ErrInvalidState}

func (service *Service) Reserve(ctx context.Context, input ReserveInput) (ReserveResult, error) {
	ctx, span := telemetry.Start(
		ctx,
		"reservation.reserve",
		attribute.String("app.show_id", input.ShowID),
		attribute.String("app.user_id", input.UserID),
		attribute.Int("app.seat_count", len(input.SeatNumbers)),
		attribute.String("app.idempotency_key", input.IdempotencyKey),
	)
	result, err := service.reserve(ctx, input)
	if err == nil {
		outcome := "confirmed"
		if result.Replayed {
			outcome = "replayed"
		}
		span.SetAttributes(
			attribute.String("app.outcome", outcome),
			attribute.String("app.reservation_id", result.Reservation.ID),
			attribute.Int64("app.amount_paise", result.Reservation.AmountPaise),
		)
	}
	telemetry.Finish(span, err, reserveDeclines...)
	return result, err
}

func step(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return telemetry.Start(ctx, "reservation."+name, attributes...)
}

func (service *Service) reserve(ctx context.Context, input ReserveInput) (ReserveResult, error) {
	sortedSeats := append([]string(nil), input.SeatNumbers...)
	sort.Strings(sortedSeats)

	tx, err := service.database.Begin(ctx)
	if err != nil {
		return ReserveResult{}, fmt.Errorf("begin reservation: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	lockContext, lockSpan := step(ctx, "lock_user_show")
	lockErr := acquireUserShowReservationLock(lockContext, tx, input.UserID, input.ShowID)
	telemetry.Finish(lockSpan, lockErr)
	if lockErr != nil {
		return ReserveResult{}, lockErr
	}

	idempotencyContext, idempotencySpan := step(ctx, "check_idempotency")
	existing, err := loadByIdempotencyKey(idempotencyContext, tx, input.UserID, input.IdempotencyKey)
	if err == nil {
		idempotencySpan.SetAttributes(attribute.Bool("app.key_found", true))
		if !sameRequest(existing, input.ShowID, sortedSeats) {
			telemetry.Finish(idempotencySpan, ErrIdempotencyConflict, ErrIdempotencyConflict)
			return ReserveResult{}, ErrIdempotencyConflict
		}
		telemetry.Finish(idempotencySpan, nil)
		if err := tx.Commit(ctx); err != nil {
			return ReserveResult{}, fmt.Errorf("commit idempotent replay: %w", err)
		}
		return ReserveResult{Reservation: existing, Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		telemetry.Finish(idempotencySpan, err)
		return ReserveResult{}, err
	}
	idempotencySpan.SetAttributes(attribute.Bool("app.key_found", false))
	telemetry.Finish(idempotencySpan, nil)

	limitContext, limitSpan := step(ctx, "check_limit")
	var pricePaise int64
	var perUserLimit int64
	var activeSeats int64
	err = tx.QueryRow(limitContext, `
		SELECT
			show.price_paise,
			show.per_user_limit,
			(
				SELECT count(*)
				FROM reservation_seats AS reservation_seat
				JOIN reservations AS reservation
					ON reservation.id = reservation_seat.reservation_id
				WHERE reservation.user_id = $2
					AND reservation.show_id = show.id
					AND reservation.status = 'confirmed'
			)
		FROM shows AS show
		WHERE show.id = $1
	`, input.ShowID, input.UserID).Scan(&pricePaise, &perUserLimit, &activeSeats)
	if errors.Is(err, pgx.ErrNoRows) {
		telemetry.Finish(limitSpan, ErrShowNotFound, ErrShowNotFound)
		return ReserveResult{}, ErrShowNotFound
	}
	if err != nil {
		wrapped := fmt.Errorf("select show and active seats: %w", err)
		telemetry.Finish(limitSpan, wrapped)
		return ReserveResult{}, wrapped
	}
	limitSpan.SetAttributes(
		attribute.Int64("app.active_seats", activeSeats),
		attribute.Int64("app.per_user_limit", perUserLimit),
		attribute.Int("app.requested_seats", len(input.SeatNumbers)),
		attribute.Int64("app.price_paise", pricePaise),
	)
	if activeSeats+int64(len(input.SeatNumbers)) > perUserLimit {
		telemetry.Finish(limitSpan, ErrUserLimitExceeded, ErrUserLimitExceeded)
		return ReserveResult{}, ErrUserLimitExceeded
	}

	if len(input.SeatNumbers) > 0 && pricePaise > math.MaxInt64/int64(len(input.SeatNumbers)) {
		telemetry.Finish(limitSpan, ErrAmountOutOfRange, ErrAmountOutOfRange)
		return ReserveResult{}, ErrAmountOutOfRange
	}
	telemetry.Finish(limitSpan, nil)
	amountPaise := pricePaise * int64(len(input.SeatNumbers))

	seatContext, seatSpan := step(ctx, "lock_seats", attribute.String("app.seats", strings.Join(sortedSeats, ",")))
	lockedSeats, err := lockAvailableSeats(seatContext, tx, input.ShowID, sortedSeats)
	if err != nil {
		telemetry.Finish(seatSpan, err)
		return ReserveResult{}, err
	}
	seatSpan.SetAttributes(
		attribute.Int("app.requested_seats", len(sortedSeats)),
		attribute.Int("app.locked_seats", len(lockedSeats)),
	)
	if len(lockedSeats) != len(sortedSeats) {
		seatSpan.SetAttributes(attribute.StringSlice("app.unavailable_seats", missingSeats(sortedSeats, lockedSeats)))
		telemetry.Finish(seatSpan, ErrSeatsUnavailable, ErrSeatsUnavailable)
		return ReserveResult{}, ErrSeatsUnavailable
	}
	telemetry.Finish(seatSpan, nil)

	insertContext, insertSpan := step(ctx, "insert_reservation")
	var created Reservation
	var createdStatus string
	err = tx.QueryRow(insertContext, `
		INSERT INTO reservations (
			show_id,
			user_id,
			idempotency_key,
			amount_paise
		)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, idempotency_key) DO NOTHING
		RETURNING id::text, show_id::text, user_id::text, amount_paise, status::text
	`, input.ShowID, input.UserID, input.IdempotencyKey, amountPaise).Scan(
		&created.ID,
		&created.ShowID,
		&created.UserID,
		&created.AmountPaise,
		&createdStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		insertSpan.SetAttributes(attribute.Bool("app.concurrent_replay", true))
		existing, loadErr := loadByIdempotencyKey(insertContext, tx, input.UserID, input.IdempotencyKey)
		if loadErr != nil {
			telemetry.Finish(insertSpan, loadErr)
			return ReserveResult{}, loadErr
		}
		if !sameRequest(existing, input.ShowID, sortedSeats) {
			telemetry.Finish(insertSpan, ErrIdempotencyConflict, ErrIdempotencyConflict)
			return ReserveResult{}, ErrIdempotencyConflict
		}
		telemetry.Finish(insertSpan, nil)
		commitContext, commitSpan := step(ctx, "commit")
		if err := tx.Commit(commitContext); err != nil {
			wrapped := fmt.Errorf("commit concurrent idempotent replay: %w", err)
			telemetry.Finish(commitSpan, wrapped)
			return ReserveResult{}, wrapped
		}
		telemetry.Finish(commitSpan, nil)
		return ReserveResult{Reservation: existing, Replayed: true}, nil
	}
	if err != nil {
		wrapped := fmt.Errorf("insert reservation: %w", err)
		telemetry.Finish(insertSpan, wrapped)
		return ReserveResult{}, wrapped
	}
	created.Status = Status(createdStatus)
	insertSpan.SetAttributes(attribute.String("app.reservation_id", created.ID))
	telemetry.Finish(insertSpan, nil)

	confirmContext, confirmSpan := step(ctx, "confirm_seats")
	if _, err := tx.Exec(confirmContext, `
		INSERT INTO reservation_seats (reservation_id, show_id, seat_number)
		SELECT $1::uuid, $2::uuid, input.seat_number
		FROM unnest($3::text[]) AS input(seat_number)
	`, created.ID, input.ShowID, sortedSeats); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) &&
			postgresError.ConstraintName == "reservation_seats_active_show_seat_unique" {
			confirmSpan.SetAttributes(attribute.String("app.constraint", postgresError.ConstraintName))
			telemetry.Finish(confirmSpan, ErrSeatsUnavailable, ErrSeatsUnavailable)
			return ReserveResult{}, ErrSeatsUnavailable
		}
		wrapped := fmt.Errorf("insert reservation seats: %w", err)
		telemetry.Finish(confirmSpan, wrapped)
		return ReserveResult{}, wrapped
	}

	updated, err := tx.Exec(confirmContext, `
		UPDATE seats
		SET state = 'confirmed'
		WHERE show_id = $1
			AND seat_number = ANY($2::text[])
			AND state = 'available'
	`, input.ShowID, sortedSeats)
	if err != nil {
		wrapped := fmt.Errorf("confirm seats: %w", err)
		telemetry.Finish(confirmSpan, wrapped)
		return ReserveResult{}, wrapped
	}
	confirmSpan.SetAttributes(attribute.Int64("app.seats_confirmed", updated.RowsAffected()))
	if updated.RowsAffected() != int64(len(sortedSeats)) {
		telemetry.Finish(confirmSpan, ErrSeatsUnavailable, ErrSeatsUnavailable)
		return ReserveResult{}, ErrSeatsUnavailable
	}
	telemetry.Finish(confirmSpan, nil)

	created.Seats = sortedSeats
	commitContext, commitSpan := step(ctx, "commit")
	if err := tx.Commit(commitContext); err != nil {
		wrapped := fmt.Errorf("commit reservation: %w", err)
		telemetry.Finish(commitSpan, wrapped)
		return ReserveResult{}, wrapped
	}
	telemetry.Finish(commitSpan, nil)

	return ReserveResult{Reservation: created}, nil
}

func lockAvailableSeats(ctx context.Context, tx pgx.Tx, showID string, sortedSeats []string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT seat_number
		FROM seats
		WHERE show_id = $1
			AND seat_number = ANY($2::text[])
			AND state = 'available'
		ORDER BY seat_number
		FOR UPDATE
	`, showID, sortedSeats)
	if err != nil {
		return nil, fmt.Errorf("lock seats: %w", err)
	}

	lockedSeats := make([]string, 0, len(sortedSeats))
	for rows.Next() {
		var seatNumber string
		if err := rows.Scan(&seatNumber); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan locked seat: %w", err)
		}
		lockedSeats = append(lockedSeats, seatNumber)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate locked seats: %w", err)
	}
	rows.Close()
	return lockedSeats, nil
}

func missingSeats(requested []string, locked []string) []string {
	present := make(map[string]struct{}, len(locked))
	for _, seat := range locked {
		present[seat] = struct{}{}
	}
	missing := make([]string, 0, len(requested))
	for _, seat := range requested {
		if _, exists := present[seat]; !exists {
			missing = append(missing, seat)
		}
	}
	return missing
}

func (service *Service) Cancel(ctx context.Context, reservationID string, userID string) (Reservation, error) {
	ctx, span := telemetry.Start(
		ctx,
		"reservation.cancel",
		attribute.String("app.reservation_id", reservationID),
		attribute.String("app.user_id", userID),
	)
	cancelled, err := service.cancel(ctx, reservationID, userID)
	if err == nil {
		span.SetAttributes(
			attribute.String("app.outcome", string(cancelled.Status)),
			attribute.String("app.show_id", cancelled.ShowID),
			attribute.Int("app.seat_count", len(cancelled.Seats)),
		)
	}
	telemetry.Finish(span, err, cancelDeclines...)
	return cancelled, err
}

func (service *Service) cancel(ctx context.Context, reservationID string, userID string) (Reservation, error) {
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return Reservation{}, fmt.Errorf("begin cancellation: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var showID string
	err = tx.QueryRow(ctx, `
		SELECT show_id::text
		FROM reservations
		WHERE id = $1
			AND user_id = $2
	`, reservationID, userID).Scan(&showID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, ErrNotFound
	}
	if err != nil {
		return Reservation{}, fmt.Errorf("select reservation owner: %w", err)
	}

	if err := acquireUserShowReservationLock(ctx, tx, userID, showID); err != nil {
		return Reservation{}, err
	}

	existing, err := loadByIDForUpdate(ctx, tx, reservationID)
	if err != nil {
		return Reservation{}, err
	}
	if existing.UserID != userID {
		return Reservation{}, ErrNotFound
	}
	if existing.Status == StatusCancelled {
		if err := tx.Commit(ctx); err != nil {
			return Reservation{}, fmt.Errorf("commit repeated cancellation: %w", err)
		}
		return existing, nil
	}
	if existing.Status != StatusConfirmed {
		return Reservation{}, ErrInvalidState
	}

	rows, err := tx.Query(ctx, `
		SELECT seat.seat_number
		FROM reservation_seats AS reservation_seat
		JOIN reservations AS reservation
			ON reservation.id = reservation_seat.reservation_id
		JOIN seats AS seat
			ON seat.show_id = reservation.show_id
			AND seat.seat_number = reservation_seat.seat_number
		WHERE reservation_seat.reservation_id = $1
		ORDER BY seat.seat_number
		FOR UPDATE OF seat
	`, reservationID)
	if err != nil {
		return Reservation{}, fmt.Errorf("lock reservation seats: %w", err)
	}
	lockedCount := 0
	for rows.Next() {
		lockedCount++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Reservation{}, fmt.Errorf("iterate reservation seats: %w", err)
	}
	rows.Close()
	if lockedCount != len(existing.Seats) {
		return Reservation{}, ErrInvalidState
	}

	released, err := tx.Exec(ctx, `
		UPDATE seats AS seat
		SET state = 'available'
		FROM reservations AS reservation, reservation_seats AS reservation_seat
		WHERE reservation.id = $1
			AND reservation_seat.reservation_id = reservation.id
			AND seat.show_id = reservation.show_id
			AND seat.seat_number = reservation_seat.seat_number
			AND seat.state = 'confirmed'
	`, reservationID)
	if err != nil {
		return Reservation{}, fmt.Errorf("release seats: %w", err)
	}
	if released.RowsAffected() != int64(len(existing.Seats)) {
		return Reservation{}, ErrInvalidState
	}

	deactivated, err := tx.Exec(ctx, `
		UPDATE reservation_seats
		SET active = false
		WHERE reservation_id = $1
			AND active
	`, reservationID)
	if err != nil {
		return Reservation{}, fmt.Errorf("deactivate reservation seats: %w", err)
	}
	if deactivated.RowsAffected() != int64(len(existing.Seats)) {
		return Reservation{}, ErrInvalidState
	}

	if _, err := tx.Exec(ctx, `
		UPDATE reservations
		SET status = 'cancelled', updated_at = now()
		WHERE id = $1
			AND status = 'confirmed'
	`, reservationID); err != nil {
		return Reservation{}, fmt.Errorf("cancel reservation: %w", err)
	}

	existing.Status = StatusCancelled
	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, fmt.Errorf("commit cancellation: %w", err)
	}
	return existing, nil
}

func acquireUserShowReservationLock(ctx context.Context, tx pgx.Tx, userID string, showID string) error {
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(
			hashtextextended('user-show-reservations:' || $1::text || ':' || $2::text, 0)
		)
	`, showID, userID); err != nil {
		return fmt.Errorf("lock user show: %w", err)
	}
	return nil
}

func loadByIdempotencyKey(
	ctx context.Context,
	tx pgx.Tx,
	userID string,
	idempotencyKey string,
) (Reservation, error) {
	var existing Reservation
	var status string
	err := tx.QueryRow(ctx, `
		SELECT
			reservation.id::text,
			reservation.show_id::text,
			reservation.user_id::text,
			reservation.amount_paise,
			reservation.status::text,
			COALESCE(
				array_agg(reservation_seat.seat_number ORDER BY reservation_seat.seat_number)
					FILTER (WHERE reservation_seat.seat_number IS NOT NULL),
				ARRAY[]::text[]
			)
		FROM reservations AS reservation
		LEFT JOIN reservation_seats AS reservation_seat
			ON reservation_seat.reservation_id = reservation.id
		WHERE reservation.user_id = $1
			AND reservation.idempotency_key = $2
		GROUP BY reservation.id
	`, userID, idempotencyKey).Scan(
		&existing.ID,
		&existing.ShowID,
		&existing.UserID,
		&existing.AmountPaise,
		&status,
		&existing.Seats,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Reservation{}, pgx.ErrNoRows
		}
		return Reservation{}, fmt.Errorf("select idempotent reservation: %w", err)
	}
	existing.Status = Status(status)
	return existing, nil
}

func loadByIDForUpdate(ctx context.Context, tx pgx.Tx, reservationID string) (Reservation, error) {
	var existing Reservation
	var status string
	err := tx.QueryRow(ctx, `
		SELECT id::text, show_id::text, user_id::text, amount_paise, status::text
		FROM reservations
		WHERE id = $1
		FOR UPDATE
	`, reservationID).Scan(
		&existing.ID,
		&existing.ShowID,
		&existing.UserID,
		&existing.AmountPaise,
		&status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, ErrNotFound
	}
	if err != nil {
		return Reservation{}, fmt.Errorf("select reservation: %w", err)
	}
	existing.Status = Status(status)

	rows, err := tx.Query(ctx, `
		SELECT seat_number
		FROM reservation_seats
		WHERE reservation_id = $1
		ORDER BY seat_number
	`, reservationID)
	if err != nil {
		return Reservation{}, fmt.Errorf("select reservation seats: %w", err)
	}
	defer rows.Close()

	existing.Seats = make([]string, 0)
	for rows.Next() {
		var seatNumber string
		if err := rows.Scan(&seatNumber); err != nil {
			return Reservation{}, fmt.Errorf("scan reservation seat: %w", err)
		}
		existing.Seats = append(existing.Seats, seatNumber)
	}
	if err := rows.Err(); err != nil {
		return Reservation{}, fmt.Errorf("iterate reservation seats: %w", err)
	}
	return existing, nil
}

func sameRequest(existing Reservation, showID string, sortedSeatNumbers []string) bool {
	if existing.ShowID != showID || len(existing.Seats) != len(sortedSeatNumbers) {
		return false
	}
	for index := range existing.Seats {
		if existing.Seats[index] != sortedSeatNumbers[index] {
			return false
		}
	}
	return true
}
