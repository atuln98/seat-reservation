package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/httpmiddleware"
	"seat-reservation/internal/metrics"
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
	metrics      *metrics.Metrics
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
	applicationMetrics *metrics.Metrics,
	authRate int,
	authBurst int,
	metricsBearerToken string,
	requestTimeout time.Duration,
) http.Handler {
	api := &API{
		database:     database,
		logger:       logger,
		users:        users,
		shows:        shows,
		reservations: reservations,
		metrics:      applicationMetrics,
	}

	router := http.NewServeMux()
	authLimiter := httpmiddleware.NewRateLimiter(authRate, authBurst)

	router.HandleFunc("GET /{$}", api.root)
	router.HandleFunc("GET /health/live", api.liveness)
	router.HandleFunc("GET /health/ready", api.readiness)
	metricsHandler := applicationMetrics.Handler()
	if metricsBearerToken != "" {
		metricsHandler = httpmiddleware.BearerToken(metricsBearerToken, metricsHandler)
	}
	router.Handle("GET /metrics", metricsHandler)
	router.Handle("POST /auth/register", authLimiter.Middleware(http.HandlerFunc(api.register)))
	router.Handle("POST /auth/login", authLimiter.Middleware(http.HandlerFunc(api.login)))
	router.Handle("GET /users/me", tokens.Middleware(http.HandlerFunc(api.currentUser)))
	router.Handle("POST /shows", tokens.Middleware(api.requireRole(auth.RoleAdmin, http.HandlerFunc(api.createShow))))
	router.HandleFunc("GET /shows/{id}", api.getShow)
	router.Handle("POST /shows/{id}/reserve", tokens.Middleware(http.HandlerFunc(api.reserveSeats)))
	router.Handle("POST /reservations/{id}/cancel", tokens.Middleware(http.HandlerFunc(api.cancelReservation)))

	handler := httpmiddleware.Recover(logger)(httpmiddleware.CaptureRoute(router))
	handler = httpmiddleware.AccessLog(logger)(handler)
	handler = httpmiddleware.Metrics(applicationMetrics)(handler)
	handler = httpmiddleware.Trace(handler)
	handler = httpmiddleware.RequestID(handler)
	handler = httpmiddleware.RequestTimeout(requestTimeout, handler)

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
		httpmiddleware.RecordRequestError(request, err)
		requestLogger(api.logger, request).Warn(
			"readiness check failed",
			"event", "readiness_check_failed",
			"error", err,
		)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
		})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func requestLogger(logger *slog.Logger, request *http.Request) *slog.Logger {
	traceContext := httpmiddleware.TraceFromContext(request.Context())
	return logger.With(
		"requestId", httpmiddleware.RequestIDFromContext(request.Context()),
		"traceId", traceContext.TraceID,
		"spanId", traceContext.SpanID,
	)
}

func (api *API) requireRole(role auth.Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, ok := auth.PrincipalFromContext(request.Context())
		if !ok {
			writeError(writer, http.StatusUnauthorized, "authentication_required")
			return
		}

		existing, err := api.users.Get(request.Context(), principal.UserID)
		if err != nil {
			if writeContextError(writer, err) {
				return
			}
			if errors.Is(err, user.ErrNotFound) {
				writeError(writer, http.StatusUnauthorized, "invalid_token")
				return
			}
			requestLogger(api.logger, request).Error(
				"authorization lookup failed",
				"event", "authorization_lookup_failed",
				"userId", principal.UserID,
				"error", err,
			)
			httpmiddleware.RecordRequestError(request, err)
			writeError(writer, http.StatusInternalServerError, "internal_error")
			return
		}
		if existing.Role != role {
			writeError(writer, http.StatusForbidden, "insufficient_privileges")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
