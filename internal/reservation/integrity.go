package reservation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const maxIntegrityDetails = 20

type IntegrityViolation struct {
	Check  string `json:"check"`
	Seat   string `json:"seat_number,omitempty"`
	UserID string `json:"user_id,omitempty"`
	Count  int64  `json:"count,omitempty"`
}

type IntegrityReport struct {
	ShowID               string               `json:"show_id"`
	OK                   bool                 `json:"ok"`
	SeatsConfirmed       int64                `json:"seats_confirmed"`
	SeatsWithActiveOwner int64                `json:"seats_with_active_owner"`
	Violations           int64                `json:"violations"`
	Details              []IntegrityViolation `json:"details"`
}

type integrityCheck struct {
	name  string
	query string
}

var integrityChecks = []integrityCheck{
	{
		name: "confirmed_seat_without_exactly_one_active_owner",
		query: `
			SELECT seat.seat_number, '', count(owner.seat_number)
			FROM seats AS seat
			LEFT JOIN reservation_seats AS owner
				ON owner.show_id = seat.show_id
				AND owner.seat_number = seat.seat_number
				AND owner.active
			WHERE seat.show_id = $1
				AND seat.state = 'confirmed'
			GROUP BY seat.seat_number
			HAVING count(owner.seat_number) <> 1
			ORDER BY seat.seat_number`,
	},
	{
		name: "active_owner_on_seat_that_is_not_confirmed",
		query: `
			SELECT owner.seat_number, '', 1
			FROM reservation_seats AS owner
			JOIN seats AS seat
				ON seat.show_id = owner.show_id
				AND seat.seat_number = owner.seat_number
			WHERE owner.show_id = $1
				AND owner.active
				AND seat.state <> 'confirmed'
			ORDER BY owner.seat_number`,
	},
	{
		name: "active_owner_on_reservation_that_is_not_confirmed",
		query: `
			SELECT owner.seat_number, reservation.user_id::text, 1
			FROM reservation_seats AS owner
			JOIN reservations AS reservation
				ON reservation.id = owner.reservation_id
			WHERE owner.show_id = $1
				AND owner.active
				AND reservation.status <> 'confirmed'
			ORDER BY owner.seat_number`,
	},
	{
		name: "confirmed_reservation_with_inactive_seat",
		query: `
			SELECT owner.seat_number, reservation.user_id::text, 1
			FROM reservation_seats AS owner
			JOIN reservations AS reservation
				ON reservation.id = owner.reservation_id
			WHERE owner.show_id = $1
				AND NOT owner.active
				AND reservation.status = 'confirmed'
			ORDER BY owner.seat_number`,
	},
	{
		name: "user_over_per_user_limit",
		query: `
			SELECT '', reservation.user_id::text, count(*)
			FROM reservation_seats AS owner
			JOIN reservations AS reservation
				ON reservation.id = owner.reservation_id
			JOIN shows AS show
				ON show.id = owner.show_id
			WHERE owner.show_id = $1
				AND owner.active
				AND reservation.status = 'confirmed'
			GROUP BY reservation.user_id, show.per_user_limit
			HAVING count(*) > show.per_user_limit
			ORDER BY reservation.user_id`,
	},
}

func (service *Service) Integrity(ctx context.Context, showID string) (IntegrityReport, error) {
	tx, err := service.database.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("begin integrity check: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	report := IntegrityReport{ShowID: showID, Details: []IntegrityViolation{}}
	err = tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM seats WHERE show_id = $1 AND state = 'confirmed'),
			(SELECT count(*) FROM reservation_seats WHERE show_id = $1 AND active)
		WHERE EXISTS (SELECT 1 FROM shows WHERE id = $1)
	`, showID).Scan(&report.SeatsConfirmed, &report.SeatsWithActiveOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return IntegrityReport{}, ErrShowNotFound
	}
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("count seats and owners: %w", err)
	}
	if report.SeatsConfirmed != report.SeatsWithActiveOwner {
		report.Violations++
		report.Details = append(report.Details, IntegrityViolation{
			Check: "confirmed_seat_count_differs_from_active_owner_count",
			Count: report.SeatsConfirmed - report.SeatsWithActiveOwner,
		})
	}

	for _, check := range integrityChecks {
		rows, err := tx.Query(ctx, check.query, showID)
		if err != nil {
			return IntegrityReport{}, fmt.Errorf("run integrity check %s: %w", check.name, err)
		}
		for rows.Next() {
			var violation IntegrityViolation
			if err := rows.Scan(&violation.Seat, &violation.UserID, &violation.Count); err != nil {
				rows.Close()
				return IntegrityReport{}, fmt.Errorf("scan integrity check %s: %w", check.name, err)
			}
			violation.Check = check.name
			report.Violations++
			if len(report.Details) < maxIntegrityDetails {
				report.Details = append(report.Details, violation)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return IntegrityReport{}, fmt.Errorf("iterate integrity check %s: %w", check.name, err)
		}
		rows.Close()
	}

	report.OK = report.Violations == 0
	return report, nil
}
