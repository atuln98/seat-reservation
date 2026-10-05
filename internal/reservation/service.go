package reservation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

func (service *Service) Reserve(ctx context.Context, input ReserveInput) (ReserveResult, error) {
	sortedSeats := append([]string(nil), input.SeatNumbers...)
	sort.Strings(sortedSeats)

	tx, err := service.database.Begin(ctx)
	if err != nil {
		return ReserveResult{}, fmt.Errorf("begin reservation: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := acquireUserShowReservationLock(ctx, tx, input.UserID, input.ShowID); err != nil {
		return ReserveResult{}, err
	}

	existing, err := loadByIdempotencyKey(ctx, tx, input.UserID, input.IdempotencyKey)
	if err == nil {
		if !sameRequest(existing, input.ShowID, sortedSeats) {
			return ReserveResult{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return ReserveResult{}, fmt.Errorf("commit idempotent replay: %w", err)
		}
		return ReserveResult{Reservation: existing, Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReserveResult{}, err
	}

	var pricePaise int64
	var perUserLimit int64
	var activeSeats int64
	err = tx.QueryRow(ctx, `
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
		return ReserveResult{}, ErrShowNotFound
	}
	if err != nil {
		return ReserveResult{}, fmt.Errorf("select show and active seats: %w", err)
	}
	if activeSeats+int64(len(input.SeatNumbers)) > perUserLimit {
		return ReserveResult{}, ErrUserLimitExceeded
	}

	if len(input.SeatNumbers) > 0 && pricePaise > math.MaxInt64/int64(len(input.SeatNumbers)) {
		return ReserveResult{}, ErrAmountOutOfRange
	}
	amountPaise := pricePaise * int64(len(input.SeatNumbers))

	rows, err := tx.Query(ctx, `
		SELECT seat_number
		FROM seats
		WHERE show_id = $1
			AND seat_number = ANY($2::text[])
			AND state = 'available'
		ORDER BY seat_number
		FOR UPDATE
	`, input.ShowID, sortedSeats)
	if err != nil {
		return ReserveResult{}, fmt.Errorf("lock seats: %w", err)
	}

	lockedSeats := make([]string, 0, len(sortedSeats))
	for rows.Next() {
		var seatNumber string
		if err := rows.Scan(&seatNumber); err != nil {
			rows.Close()
			return ReserveResult{}, fmt.Errorf("scan locked seat: %w", err)
		}
		lockedSeats = append(lockedSeats, seatNumber)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ReserveResult{}, fmt.Errorf("iterate locked seats: %w", err)
	}
	rows.Close()
	if len(lockedSeats) != len(sortedSeats) {
		return ReserveResult{}, ErrSeatsUnavailable
	}

	var created Reservation
	var createdStatus string
	err = tx.QueryRow(ctx, `
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
		existing, loadErr := loadByIdempotencyKey(ctx, tx, input.UserID, input.IdempotencyKey)
		if loadErr != nil {
			return ReserveResult{}, loadErr
		}
		if !sameRequest(existing, input.ShowID, sortedSeats) {
			return ReserveResult{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return ReserveResult{}, fmt.Errorf("commit concurrent idempotent replay: %w", err)
		}
		return ReserveResult{Reservation: existing, Replayed: true}, nil
	}
	if err != nil {
		return ReserveResult{}, fmt.Errorf("insert reservation: %w", err)
	}
	created.Status = Status(createdStatus)

	if _, err := tx.Exec(ctx, `
		INSERT INTO reservation_seats (reservation_id, show_id, seat_number)
		SELECT $1::uuid, $2::uuid, input.seat_number
		FROM unnest($3::text[]) AS input(seat_number)
	`, created.ID, input.ShowID, sortedSeats); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) &&
			postgresError.ConstraintName == "reservation_seats_active_show_seat_unique" {
			return ReserveResult{}, ErrSeatsUnavailable
		}
		return ReserveResult{}, fmt.Errorf("insert reservation seats: %w", err)
	}

	updated, err := tx.Exec(ctx, `
		UPDATE seats
		SET state = 'confirmed'
		WHERE show_id = $1
			AND seat_number = ANY($2::text[])
			AND state = 'available'
	`, input.ShowID, sortedSeats)
	if err != nil {
		return ReserveResult{}, fmt.Errorf("confirm seats: %w", err)
	}
	if updated.RowsAffected() != int64(len(sortedSeats)) {
		return ReserveResult{}, ErrSeatsUnavailable
	}

	created.Seats = sortedSeats
	if err := tx.Commit(ctx); err != nil {
		return ReserveResult{}, fmt.Errorf("commit reservation: %w", err)
	}

	return ReserveResult{Reservation: created}, nil
}

func (service *Service) Cancel(ctx context.Context, reservationID string, userID string) (Reservation, error) {
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
