package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/httpmiddleware"
	"seat-reservation/internal/user"

	"github.com/jackc/pgx/v5/pgxpool"
)

type API struct {
	database *pgxpool.Pool
	logger   *slog.Logger
	users    userOperations
}

type userOperations interface {
	Register(context.Context, string, string) (user.Authentication, error)
	Login(context.Context, string, string) (user.Authentication, error)
	Get(context.Context, string) (user.User, error)
}

func NewRouter(
	database *pgxpool.Pool,
	logger *slog.Logger,
	tokens *auth.TokenManager,
	users *user.Service,
) http.Handler {
	api := &API{
		database: database,
		logger:   logger,
		users:    users,
	}

	router := http.NewServeMux()

	router.HandleFunc("GET /{$}", api.root)
	router.HandleFunc("GET /health/live", api.liveness)
	router.HandleFunc("GET /health/ready", api.readiness)
	router.HandleFunc("POST /auth/register", api.register)
	router.HandleFunc("POST /auth/login", api.login)
	router.Handle("GET /users/me", tokens.Middleware(http.HandlerFunc(api.currentUser)))

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
