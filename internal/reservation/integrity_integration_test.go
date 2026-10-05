package reservation_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"seat-reservation/internal/database"
	"seat-reservation/internal/reservation"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIntegrityDetectsCorruptedOwnership(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool, "../../migrations"); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	suffix := time.Now().UnixNano()
	var showID, userID string
	err = pool.QueryRow(ctx, `
		INSERT INTO shows (name, price_paise, per_user_limit)
		VALUES ($1, 1000, 4)
		RETURNING id::text
	`, fmt.Sprintf("integrity-%d", suffix)).Scan(&showID)
	if err != nil {
		t.Fatalf("create show: %v", err)
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, 'integration-test')
		RETURNING id::text
	`, fmt.Sprintf("integrity-%d@example.test", suffix)).Scan(&userID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM reservation_seats WHERE show_id = $1`, showID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM reservations WHERE show_id = $1`, showID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM shows WHERE id = $1`, showID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	}()
	if _, err := pool.Exec(ctx, `
		INSERT INTO seats (show_id, seat_number)
		VALUES ($1, 'A1'), ($1, 'A2'), ($1, 'A3')
	`, showID); err != nil {
		t.Fatalf("create seats: %v", err)
	}

	service := reservation.NewService(pool)
	for index, seat := range []string{"A1", "A3"} {
		if _, err := service.Reserve(ctx, reservation.ReserveInput{
			ShowID:         showID,
			UserID:         userID,
			SeatNumbers:    []string{seat},
			IdempotencyKey: fmt.Sprintf("integrity-%d-%d", suffix, index),
		}); err != nil {
			t.Fatalf("reserve %s: %v", seat, err)
		}
	}

	report, err := service.Integrity(ctx, showID)
	if err != nil {
		t.Fatalf("integrity check: %v", err)
	}
	if !report.OK || report.Violations != 0 || report.SeatsConfirmed != 2 || report.SeatsWithActiveOwner != 2 {
		t.Fatalf("clean state reported %+v", report)
	}

	corruptions := []struct {
		name    string
		damage  string
		restore string
		check   string
	}{
		{
			name:    "confirmed seat nobody owns",
			damage:  `UPDATE seats SET state = 'confirmed' WHERE show_id = $1 AND seat_number = 'A2'`,
			restore: `UPDATE seats SET state = 'available' WHERE show_id = $1 AND seat_number = 'A2'`,
			check:   "confirmed_seat_without_exactly_one_active_owner",
		},
		{
			name:    "owned seat marked available",
			damage:  `UPDATE seats SET state = 'available' WHERE show_id = $1 AND seat_number = 'A1'`,
			restore: `UPDATE seats SET state = 'confirmed' WHERE show_id = $1 AND seat_number = 'A1'`,
			check:   "active_owner_on_seat_that_is_not_confirmed",
		},
		{
			name:    "user over the per-user limit",
			damage:  `UPDATE shows SET per_user_limit = 1 WHERE id = $1`,
			restore: `UPDATE shows SET per_user_limit = 4 WHERE id = $1`,
			check:   "user_over_per_user_limit",
		},
	}
	for _, corruption := range corruptions {
		if _, err := pool.Exec(ctx, corruption.damage, showID); err != nil {
			t.Fatalf("%s: damage: %v", corruption.name, err)
		}
		damaged, err := service.Integrity(ctx, showID)
		if _, restoreErr := pool.Exec(ctx, corruption.restore, showID); restoreErr != nil {
			t.Fatalf("%s: restore: %v", corruption.name, restoreErr)
		}
		if err != nil {
			t.Fatalf("%s: integrity check: %v", corruption.name, err)
		}
		found := false
		for _, detail := range damaged.Details {
			if detail.Check == corruption.check {
				found = true
			}
		}
		if damaged.OK || !found {
			t.Fatalf("%s: corruption was not detected: %+v", corruption.name, damaged)
		}
	}

	restored, err := service.Integrity(ctx, showID)
	if err != nil || !restored.OK {
		t.Fatalf("restored state reported %+v, error %v", restored, err)
	}

	if _, err := service.Integrity(ctx, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("unknown show did not return an error")
	}
}
