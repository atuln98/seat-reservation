package user

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"seat-reservation/internal/auth"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrEmailExists        = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidEmail       = errors.New("invalid email")
	ErrInvalidPassword    = errors.New("password must contain between 8 and 72 bytes")
	ErrNotFound           = errors.New("user not found")
)

type User struct {
	ID    string    `json:"user_id"`
	Email string    `json:"email"`
	Role  auth.Role `json:"role"`
}

type Authentication struct {
	User
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

type Service struct {
	database *pgxpool.Pool
	tokens   *auth.TokenManager
}

func NewService(database *pgxpool.Pool, tokens *auth.TokenManager) *Service {
	return &Service{database: database, tokens: tokens}
}

func (service *Service) Register(ctx context.Context, email string, password string) (Authentication, error) {
	normalizedEmail, err := NormalizeEmail(email)
	if err != nil {
		return Authentication{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return Authentication{}, err
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return Authentication{}, fmt.Errorf("hash password: %w", err)
	}

	var created User
	err = service.database.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING id::text, email, role::text
	`, normalizedEmail, string(passwordHash)).Scan(&created.ID, &created.Email, &created.Role)
	if err != nil {
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) && databaseError.Code == "23505" {
			return Authentication{}, ErrEmailExists
		}
		return Authentication{}, fmt.Errorf("insert user: %w", err)
	}

	return service.authentication(created)
}

func (service *Service) Login(ctx context.Context, email string, password string) (Authentication, error) {
	normalizedEmail, err := NormalizeEmail(email)
	if err != nil {
		return Authentication{}, ErrInvalidCredentials
	}

	var existing User
	var passwordHash string
	err = service.database.QueryRow(ctx, `
		SELECT id::text, email, password_hash, role::text
		FROM users
		WHERE lower(email) = $1
	`, normalizedEmail).Scan(&existing.ID, &existing.Email, &passwordHash, &existing.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Authentication{}, ErrInvalidCredentials
	}
	if err != nil {
		return Authentication{}, fmt.Errorf("select user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return Authentication{}, ErrInvalidCredentials
	}

	return service.authentication(existing)
}

func (service *Service) Get(ctx context.Context, userID string) (User, error) {
	var existing User
	err := service.database.QueryRow(ctx, `
		SELECT id::text, email, role::text
		FROM users
		WHERE id = $1
	`, userID).Scan(&existing.ID, &existing.Email, &existing.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("select user: %w", err)
	}
	return existing, nil
}

func (service *Service) BootstrapAdmin(ctx context.Context, email string, password string) error {
	normalizedEmail, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}

	_, err = service.database.Exec(ctx, `
		INSERT INTO users (email, password_hash, role)
		VALUES ($1, $2, 'admin')
		ON CONFLICT (lower(email))
		DO UPDATE SET
			password_hash = EXCLUDED.password_hash,
			role = 'admin',
			updated_at = now()
	`, normalizedEmail, string(passwordHash))
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	return nil
}

func (service *Service) authentication(existing User) (Authentication, error) {
	token, err := service.tokens.Issue(existing.ID, existing.Role)
	if err != nil {
		return Authentication{}, err
	}
	return Authentication{
		User:        existing,
		AccessToken: token,
		ExpiresIn:   service.tokens.LifetimeSeconds(),
	}, nil
}

func NormalizeEmail(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(normalized)
	if err != nil || address.Address != normalized {
		return "", ErrInvalidEmail
	}
	return normalized, nil
}

func ValidatePassword(password string) error {
	length := len([]byte(password))
	if length < 8 || length > 72 {
		return ErrInvalidPassword
	}
	return nil
}
