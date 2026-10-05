package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/httpmiddleware"
	"seat-reservation/internal/reservation"
	"seat-reservation/internal/show"
	"seat-reservation/internal/user"

	"github.com/jackc/pgx/v5/pgxpool"
)

type API struct {
	database     *pgxpool.Pool
	logger       *slog.Logger
	users        userOperations
	shows        showOperations
	reservations reservationOperations
}

type userOperations interface {
	Register(context.Context, string, string) (user.Authentication, error)
	Login(context.Context, string, string) (user.Authentication, error)
	Get(context.Context, string) (user.User, error)
}

type showOperations interface {
	Create(context.Context, show.CreateInput) (show.Show, error)
	Get(context.Context, string) (show.Show, error)
}

type reservationOperations interface {
	Reserve(context.Context, reservation.ReserveInput) (reservation.ReserveResult, error)
	Cancel(context.Context, string, string) (reservation.Reservation, error)
}

func NewRouter(
	database *pgxpool.Pool,
	logger *slog.Logger,
	tokens *auth.TokenManager,
	users *user.Service,
	shows *show.Service,
	reservations *reservation.Service,
	authRate int,
	authBurst int,
) http.Handler {
	api := &API{
		database:     database,
		logger:       logger,
		users:        users,
		shows:        shows,
		reservations: reservations,
	}

	router := http.NewServeMux()
	authLimiter := httpmiddleware.NewRateLimiter(authRate, authBurst)

	router.HandleFunc("GET /{$}", api.root)
	router.HandleFunc("GET /health/live", api.liveness)
	router.HandleFunc("GET /health/ready", api.readiness)
	router.Handle("POST /auth/register", authLimiter.Middleware(http.HandlerFunc(api.register)))
	router.Handle("POST /auth/login", authLimiter.Middleware(http.HandlerFunc(api.login)))
	router.Handle("GET /users/me", tokens.Middleware(http.HandlerFunc(api.currentUser)))
	router.Handle("POST /shows", tokens.RequireRole(auth.RoleAdmin, http.HandlerFunc(api.createShow)))
	router.HandleFunc("GET /shows/{id}", api.getShow)
	router.Handle("POST /shows/{id}/reserve", tokens.Middleware(http.HandlerFunc(api.reserveSeats)))
	router.Handle("POST /reservations/{id}/cancel", tokens.Middleware(http.HandlerFunc(api.cancelReservation)))

	handler := httpmiddleware.Recover(logger)(router)
	handler = httpmiddleware.AccessLog(logger)(handler)
	handler = httpmiddleware.RequestID(handler)

	return handler
}

func (api *API) root(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{
		"name":   "seat-reservation",
		"status": "running",
	})
}

func (api *API) liveness(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "alive"})
}

func (api *API) readiness(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), time.Second)
	defer cancel()

	if err := api.database.Ping(ctx); err != nil {
		api.logger.Warn(
			"readiness check failed",
			"request_id", httpmiddleware.RequestIDFromContext(request.Context()),
			"error", err,
		)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
		})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
