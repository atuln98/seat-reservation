package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"seat-reservation/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
)

type principalKey struct{}

func (manager *TokenManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, span := telemetry.Start(request.Context(), "auth.authenticate")
		header := request.Header.Get("Authorization")
		scheme, token, found := strings.Cut(header, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
			span.SetAttributes(attribute.String("app.outcome", "rejected"), attribute.String("app.reject_reason", "missing_bearer_token"))
			span.End()
			writeAuthError(writer, http.StatusUnauthorized, "authentication_required")
			return
		}

		principal, err := manager.Parse(token)
		if err != nil {
			span.SetAttributes(attribute.String("app.outcome", "rejected"), attribute.String("app.reject_reason", err.Error()))
			span.End()
			writeAuthError(writer, http.StatusUnauthorized, "invalid_token")
			return
		}
		span.SetAttributes(
			attribute.String("app.outcome", "authenticated"),
			attribute.String("app.user_id", principal.UserID),
			attribute.String("app.role", string(principal.Role)),
		)
		span.End()

		ctx := context.WithValue(request.Context(), principalKey{}, principal)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (manager *TokenManager) RequireRole(role Role, next http.Handler) http.Handler {
	return manager.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, ok := PrincipalFromContext(request.Context())
		if !ok || principal.Role != role {
			writeAuthError(writer, http.StatusForbidden, "insufficient_privileges")
			return
		}
		next.ServeHTTP(writer, request)
	}))
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok
}

func writeAuthError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": code})
}
