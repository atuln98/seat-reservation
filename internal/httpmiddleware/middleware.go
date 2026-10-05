package httpmiddleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type requestIDKey struct{}
type traceContextKey struct{}

type TraceContext struct {
	TraceID string
	SpanID  string
}

type RequestObserver interface {
	ObserveRequest(string, string, int, time.Duration)
}

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (writer *responseWriter) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *responseWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	count, err := writer.ResponseWriter.Write(body)
	writer.bytes += count
	return count, err
}

func Metrics(observer RequestObserver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			startedAt := time.Now()
			wrapped := &responseWriter{ResponseWriter: writer}
			next.ServeHTTP(wrapped, request)

			status := wrapped.status
			if status == 0 {
				status = http.StatusOK
			}
			observer.ObserveRequest(request.Method, request.Pattern, status, time.Since(startedAt))
		})
	}
}

func BearerToken(token string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := []byte(request.Header.Get("Authorization"))
		if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writeError(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func RequestTimeout(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-ID")
		if !validRequestID(requestID) {
			requestID = newRequestID()
		}
		writer.Header().Set("X-Request-ID", requestID)
		contextWithID := context.WithValue(request.Context(), requestIDKey{}, requestID)
		next.ServeHTTP(writer, request.WithContext(contextWithID))
	})
}

func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		traceID := traceIDFromTraceparent(request.Header.Get("Traceparent"))
		if traceID == "" {
			traceID = randomHex(16)
		}
		traceContext := TraceContext{
			TraceID: traceID,
			SpanID:  randomHex(8),
		}
		writer.Header().Set("Traceparent", "00-"+traceContext.TraceID+"-"+traceContext.SpanID+"-01")
		writer.Header().Set("X-Trace-ID", traceContext.TraceID)
		ctx := context.WithValue(request.Context(), traceContextKey{}, traceContext)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error(
						"request panic",
						"event", "request_panic",
						"requestId", RequestIDFromContext(request.Context()),
						"traceId", TraceFromContext(request.Context()).TraceID,
						"spanId", TraceFromContext(request.Context()).SpanID,
						"panic", recovered,
						"stack", string(debug.Stack()),
					)
					writeError(writer, http.StatusInternalServerError, "internal_error")
				}
			}()
			next.ServeHTTP(writer, request)
		})
	}
}

func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			startedAt := time.Now()
			wrapped := &responseWriter{ResponseWriter: writer}
			next.ServeHTTP(wrapped, request)

			status := wrapped.status
			if status == 0 {
				status = http.StatusOK
			}
			logger.Info(
				"http request completed",
				"event", "http_request_completed",
				"requestId", RequestIDFromContext(request.Context()),
				"traceId", TraceFromContext(request.Context()).TraceID,
				"spanId", TraceFromContext(request.Context()).SpanID,
				"method", request.Method,
				"route", request.Pattern,
				"path", request.URL.Path,
				"status", status,
				"bytes", wrapped.bytes,
				"durationMs", time.Since(startedAt).Milliseconds(),
				"clientIp", clientIP(request),
			)
		})
	}
}

func RequestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

func TraceFromContext(ctx context.Context) TraceContext {
	traceContext, _ := ctx.Value(traceContextKey{}).(TraceContext)
	return traceContext
}

func newRequestID() string {
	return randomHex(16)
}

func validRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' ||
			character == '_' ||
			character == '.' {
			continue
		}
		return false
	}
	return true
}

func traceIDFromTraceparent(value string) string {
	parts := strings.Split(value, "-")
	if len(parts) != 4 ||
		parts[0] != "00" ||
		len(parts[1]) != 32 ||
		len(parts[2]) != 16 ||
		len(parts[3]) != 2 ||
		allZero(parts[1]) ||
		allZero(parts[2]) {
		return ""
	}
	for _, part := range parts {
		if _, err := hex.DecodeString(part); err != nil {
			return ""
		}
	}
	return strings.ToLower(parts[1])
}

func allZero(value string) bool {
	return strings.Trim(value, "0") == ""
}

func randomHex(size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err == nil {
		return hex.EncodeToString(value)
	}
	fallback := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(fallback[:size])
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": code})
}
