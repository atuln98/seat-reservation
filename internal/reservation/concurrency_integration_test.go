package reservation_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"seat-reservation/internal/database"
	"seat-reservation/internal/reservation"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConcurrentReservationsHaveOneWinner(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse database configuration: %v", err)
	}
	poolConfig.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()

	migrationErrors := make(chan error, 4)
	var migrationWait sync.WaitGroup
	for range 4 {
		migrationWait.Add(1)
		go func() {
			defer migrationWait.Done()
			migrationErrors <- database.Migrate(ctx, pool, "../../migrations")
		}()
	}
	migrationWait.Wait()
	close(migrationErrors)
	for err := range migrationErrors {
		if err != nil {
			t.Fatalf("migrate database: %v", err)
		}
	}

	suffix := time.Now().UnixNano()
	var showID string
	err = pool.QueryRow(ctx, `
		INSERT INTO shows (name, price_paise, per_user_limit)
		VALUES ($1, 1000, 4)
		RETURNING id::text
	`, fmt.Sprintf("concurrency-%d", suffix)).Scan(&showID)
	if err != nil {
		t.Fatalf("create show: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM reservation_seats WHERE show_id = $1`, showID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM reservations WHERE show_id = $1`, showID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM shows WHERE id = $1`, showID)
	}()

	if _, err := pool.Exec(ctx, `
		INSERT INTO seats (show_id, seat_number)
		VALUES ($1, 'HOT')
	`, showID); err != nil {
		t.Fatalf("create seat: %v", err)
	}

	const contenders = 64
	userIDs := make([]string, contenders)
	for index := range contenders {
		err := pool.QueryRow(ctx, `
			INSERT INTO users (email, password_hash)
			VALUES ($1, 'integration-test')
			RETURNING id::text
		`, fmt.Sprintf("concurrency-%d-%d@example.test", suffix, index)).Scan(&userIDs[index])
		if err != nil {
			t.Fatalf("create user %d: %v", index, err)
		}
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1::uuid[])`, userIDs)
	}()

	service := reservation.NewService(pool)
	start := make(chan struct{})
	type outcome struct {
		result reservation.ReserveResult
		err    error
	}
	results := make(chan outcome, contenders)
	var wait sync.WaitGroup
	for index, userID := range userIDs {
		wait.Add(1)
		go func(index int, userID string) {
			defer wait.Done()
			<-start
			result, err := service.Reserve(ctx, reservation.ReserveInput{
				ShowID:         showID,
				UserID:         userID,
				SeatNumbers:    []string{"HOT"},
				IdempotencyKey: fmt.Sprintf("concurrency-%d-%d", suffix, index),
			})
			results <- outcome{result: result, err: err}
		}(index, userID)
	}
	close(start)
	wait.Wait()
	close(results)

	confirmed := 0
	declined := 0
	var winner reservation.Reservation
	for outcome := range results {
		switch {
		case outcome.err == nil:
			confirmed++
			winner = outcome.result.Reservation
		case errors.Is(outcome.err, reservation.ErrSeatsUnavailable):
			declined++
		default:
			t.Fatalf("unexpected reservation error: %v", outcome.err)
		}
	}
	if confirmed != 1 || declined != contenders-1 {
		t.Fatalf("confirmed = %d, declined = %d", confirmed, declined)
	}

	var seatState string
	var confirmedReservations int
	var activeAssignments int
	err = pool.QueryRow(ctx, `
		SELECT
			(SELECT state::text FROM seats WHERE show_id = $1 AND seat_number = 'HOT'),
			(SELECT count(*) FROM reservations WHERE show_id = $1 AND status = 'confirmed'),
			(SELECT count(*) FROM reservation_seats WHERE show_id = $1 AND active)
	`, showID).Scan(&seatState, &confirmedReservations, &activeAssignments)
	if err != nil {
		t.Fatalf("read reconciliation: %v", err)
	}
	if seatState != "confirmed" || confirmedReservations != 1 || activeAssignments != 1 {
		t.Fatalf(
			"state = %q, confirmed reservations = %d, active assignments = %d",
			seatState,
			confirmedReservations,
			activeAssignments,
		)
	}

	if _, err := service.Cancel(ctx, winner.ID, winner.UserID); err != nil {
		t.Fatalf("cancel winning reservation: %v", err)
	}
	if _, err := service.Reserve(ctx, reservation.ReserveInput{
		ShowID:         showID,
		UserID:         userIDs[0],
		SeatNumbers:    []string{"HOT"},
		IdempotencyKey: fmt.Sprintf("rebook-%d", suffix),
	}); err != nil {
		t.Fatalf("rebook released seat: %v", err)
	}

	var historicalAssignments int
	err = pool.QueryRow(ctx, `
		SELECT count(*)
		FROM reservation_seats
		WHERE show_id = $1
	`, showID).Scan(&historicalAssignments)
	if err != nil {
		t.Fatalf("count historical assignments: %v", err)
	}
	if historicalAssignments != 2 {
		t.Fatalf("historical assignments = %d, want 2", historicalAssignments)
	}

	report, err := service.Integrity(ctx, showID)
	if err != nil {
		t.Fatalf("integrity check: %v", err)
	}
	if !report.OK || report.SeatsConfirmed != 1 || report.SeatsWithActiveOwner != 1 {
		t.Fatalf("integrity report after concurrent bookings = %+v", report)
	}
}
