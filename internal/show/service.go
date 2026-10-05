package show

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultPerUserLimit = 4

var ErrNotFound = errors.New("show not found")

type SeatState string

const (
	SeatAvailable SeatState = "available"
	SeatHeld      SeatState = "held"
	SeatConfirmed SeatState = "confirmed"
)

type Seat struct {
	Number string    `json:"seat_number"`
	State  SeatState `json:"state"`
}

type Counts struct {
	Available int `json:"available"`
	Held      int `json:"held"`
	Confirmed int `json:"confirmed"`
	Total     int `json:"total_seats"`
}

type Show struct {
	ID           string `json:"show_id"`
	Name         string `json:"name"`
	PricePaise   int64  `json:"price_paise"`
	PerUserLimit int    `json:"per_user_limit"`
	Seats        []Seat `json:"seats"`
	Counts       Counts `json:"counts"`
}

type CreateInput struct {
	Name         string
	SeatNumbers  []string
	PricePaise   int64
	PerUserLimit int
}

type Service struct {
	database *pgxpool.Pool
}

func NewService(database *pgxpool.Pool) *Service {
	return &Service{database: database}
}

func (service *Service) Create(ctx context.Context, input CreateInput) (Show, error) {
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return Show{}, fmt.Errorf("begin show creation: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var created Show
	err = tx.QueryRow(ctx, `
		INSERT INTO shows (name, price_paise, per_user_limit)
		VALUES ($1, $2, $3)
		RETURNING id::text, name, price_paise, per_user_limit
	`, input.Name, input.PricePaise, input.PerUserLimit).Scan(
		&created.ID,
		&created.Name,
		&created.PricePaise,
		&created.PerUserLimit,
	)
	if err != nil {
		return Show{}, fmt.Errorf("insert show: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO seats (show_id, seat_number, position)
		SELECT $1::uuid, input.seat_number, input.position::integer
		FROM unnest($2::text[]) WITH ORDINALITY AS input(seat_number, position)
	`, created.ID, input.SeatNumbers); err != nil {
		return Show{}, fmt.Errorf("insert show seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Show{}, fmt.Errorf("commit show creation: %w", err)
	}

	created.Seats = make([]Seat, len(input.SeatNumbers))
	for index, number := range input.SeatNumbers {
		created.Seats[index] = Seat{Number: number, State: SeatAvailable}
	}
	created.Counts = Counts{
		Available: len(created.Seats),
		Total:     len(created.Seats),
	}
	return created, nil
}

func (service *Service) Get(ctx context.Context, showID string) (Show, error) {
	var existing Show
	err := service.database.QueryRow(ctx, `
		SELECT id::text, name, price_paise, per_user_limit
		FROM shows
		WHERE id = $1
	`, showID).Scan(
		&existing.ID,
		&existing.Name,
		&existing.PricePaise,
		&existing.PerUserLimit,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Show{}, ErrNotFound
	}
	if err != nil {
		return Show{}, fmt.Errorf("select show: %w", err)
	}

	rows, err := service.database.Query(ctx, `
		SELECT seat_number, state::text
		FROM seats
		WHERE show_id = $1
		ORDER BY position
	`, showID)
	if err != nil {
		return Show{}, fmt.Errorf("select show seats: %w", err)
	}
	defer rows.Close()

	existing.Seats = make([]Seat, 0)
	for rows.Next() {
		var seat Seat
		var state string
		if err := rows.Scan(&seat.Number, &state); err != nil {
			return Show{}, fmt.Errorf("scan show seat: %w", err)
		}
		seat.State = SeatState(state)
		existing.Seats = append(existing.Seats, seat)
		switch seat.State {
		case SeatAvailable:
			existing.Counts.Available++
		case SeatHeld:
			existing.Counts.Held++
		case SeatConfirmed:
			existing.Counts.Confirmed++
		default:
			return Show{}, fmt.Errorf("unknown seat state %q", seat.State)
		}
	}
	if err := rows.Err(); err != nil {
		return Show{}, fmt.Errorf("iterate show seats: %w", err)
	}
	existing.Counts.Total = len(existing.Seats)

	return existing, nil
}
