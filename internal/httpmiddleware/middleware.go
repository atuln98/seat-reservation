package httpmiddleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type requestIDKey struct{}
type requestOutcomeKey struct{}

type TraceContext struct {
	TraceID string
	SpanID  string
}

type requestOutcome struct {
	name       string
	attributes []any
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
		ctx := propagation.TraceContext{}.Extract(request.Context(), propagation.HeaderCarrier(request.Header))
		ctx, span := otel.Tracer("seat-reservation/http").Start(
			ctx,
			request.Method+" "+request.URL.Path,
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
			oteltrace.WithAttributes(
				attribute.String("http.request.method", request.Method),
				attribute.String("url.path", request.URL.Path),
				attribute.String("request.id", RequestIDFromContext(request.Context())),
			),
		)
		spanContext := span.SpanContext()
		if !spanContext.IsValid() {
			traceID, _ := oteltrace.TraceIDFromHex(randomHex(16))
			spanID, _ := oteltrace.SpanIDFromHex(randomHex(8))
			spanContext = oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
				TraceID:    traceID,
				SpanID:     spanID,
				TraceFlags: oteltrace.FlagsSampled,
			})
			ctx = oteltrace.ContextWithSpanContext(ctx, spanContext)
		}
		writer.Header().Set("Traceparent", fmt.Sprintf(
			"00-%s-%s-%02x",
			spanContext.TraceID(),
			spanContext.SpanID(),
			byte(spanContext.TraceFlags()),
		))
		writer.Header().Set("X-Trace-ID", spanContext.TraceID().String())
		wrapped := &responseWriter{ResponseWriter: writer}
		tracedRequest := request.WithContext(ctx)
		next.ServeHTTP(wrapped, tracedRequest)

		status := wrapped.status
		if status == 0 {
			status = http.StatusOK
		}
		if tracedRequest.Pattern != "" {
			span.SetName(request.Method + " " + tracedRequest.Pattern)
		}
		span.SetAttributes(
			attribute.String("http.route", tracedRequest.Pattern),
			attribute.Int("http.response.status_code", status),
			attribute.Int("http.response.body.size", wrapped.bytes),
		)
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		span.End()
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
			traceContext := TraceFromContext(request.Context())
			requestID := RequestIDFromContext(request.Context())
			outcome := &requestOutcome{}
			request = request.WithContext(context.WithValue(request.Context(), requestOutcomeKey{}, outcome))
			wrapped := &responseWriter{ResponseWriter: writer}
			next.ServeHTTP(wrapped, request)

			status := wrapped.status
			if status == 0 {
				status = http.StatusOK
			}
			attributes := []any{
				"event", "http_request_completed",
				"requestId", requestID,
				"traceId", traceContext.TraceID,
				"spanId", traceContext.SpanID,
				"method", request.Method,
				"route", request.Pattern,
				"path", request.URL.Path,
				"status", status,
				"bytes", wrapped.bytes,
				"durationMs", time.Since(startedAt).Milliseconds(),
				"clientIp", clientIP(request),
			}
			if outcome.name != "" {
				attributes = append(attributes, "outcome", outcome.name)
				attributes = append(attributes, outcome.attributes...)
			}
			logger.Info("http request completed", attributes...)
		})
	}
}

func RecordRequestOutcome(request *http.Request, name string, attributes ...any) {
	outcome, _ := request.Context().Value(requestOutcomeKey{}).(*requestOutcome)
	if outcome == nil {
		return
	}
	outcome.name = name
	outcome.attributes = append(outcome.attributes[:0], attributes...)
	spanAttributes := make([]attribute.KeyValue, 0, len(attributes)/2)
	for index := 0; index+1 < len(attributes); index += 2 {
		key, ok := attributes[index].(string)
		if !ok {
			continue
		}
		switch value := attributes[index+1].(type) {
		case string:
			spanAttributes = append(spanAttributes, attribute.String(key, value))
		case int:
			spanAttributes = append(spanAttributes, attribute.Int(key, value))
		case int64:
			spanAttributes = append(spanAttributes, attribute.Int64(key, value))
		case bool:
			spanAttributes = append(spanAttributes, attribute.Bool(key, value))
		case []string:
			spanAttributes = append(spanAttributes, attribute.StringSlice(key, value))
		}
	}
	oteltrace.SpanFromContext(request.Context()).AddEvent(name, oteltrace.WithAttributes(spanAttributes...))
}

func RequestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

func TraceFromContext(ctx context.Context) TraceContext {
	spanContext := oteltrace.SpanContextFromContext(ctx)
	return TraceContext{
		TraceID: spanContext.TraceID().String(),
		SpanID:  spanContext.SpanID().String(),
	}
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
