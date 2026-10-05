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
	"strings"
	"time"

	"seat-reservation/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type requestIDKey struct{}
type requestOutcomeKey struct{}
type routeKey struct{}

type routeHolder struct {
	pattern string
}

type TraceContext struct {
	TraceID string
	SpanID  string
}

type requestOutcome struct {
	name       string
	generic    bool
	err        string
	attributes []any
}

type RequestObserver interface {
	ObserveRequest(string, string, int, time.Duration)
}

type responseWriter struct {
	http.ResponseWriter
	status    int
	bytes     int
	errorCode string
}

func (writer *responseWriter) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *responseWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if writer.status >= http.StatusBadRequest && writer.errorCode == "" {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &payload) == nil {
			writer.errorCode = payload.Error
		}
	}
	count, err := writer.ResponseWriter.Write(body)
	writer.bytes += count
	return count, err
}

func CaptureRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if holder, ok := request.Context().Value(routeKey{}).(*routeHolder); ok {
				holder.pattern = request.Pattern
			}
		}()
		next.ServeHTTP(writer, request)
	})
}

func RoutePattern(request *http.Request) string {
	if holder, ok := request.Context().Value(routeKey{}).(*routeHolder); ok && holder.pattern != "" {
		return holder.pattern
	}
	return request.Pattern
}

func quietRoute(pattern string) bool {
	switch pattern {
	case "GET /health/live", "GET /health/ready", "GET /metrics", "GET /{$}":
		return true
	}
	return false
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
			observer.ObserveRequest(request.Method, RoutePattern(request), status, time.Since(startedAt))
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
		ctx := context.WithValue(request.Context(), routeKey{}, &routeHolder{})
		ctx = propagation.TraceContext{}.Extract(ctx, propagation.HeaderCarrier(request.Header))
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
		route := RoutePattern(tracedRequest)
		if route != "" {
			span.SetName(route)
		}
		span.SetAttributes(
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", status),
			attribute.Int("http.response.body.size", wrapped.bytes),
		)
		if wrapped.errorCode != "" {
			span.SetAttributes(attribute.String("error.code", wrapped.errorCode))
		}
		switch {
		case status >= http.StatusInternalServerError:
			span.SetStatus(codes.Error, statusDescription(status, wrapped.errorCode))
		case status == http.StatusRequestTimeout || status == statusClientClosedRequest:
			span.SetAttributes(attribute.String("error.type", wrapped.errorCode))
			span.SetStatus(codes.Error, statusDescription(status, wrapped.errorCode))
		}
		span.End()
	})
}

const statusClientClosedRequest = 499

func statusDescription(status int, errorCode string) string {
	if errorCode != "" {
		return errorCode
	}
	return http.StatusText(status)
}

func RecordRequestError(request *http.Request, err error) {
	if err == nil {
		return
	}
	if outcome, _ := request.Context().Value(requestOutcomeKey{}).(*requestOutcome); outcome != nil {
		outcome.err = err.Error()
	}
	telemetry.RecordFailure(oteltrace.SpanFromContext(request.Context()), err)
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
					telemetry.RecordFailure(
						oteltrace.SpanFromContext(request.Context()),
						fmt.Errorf("panic: %v", recovered),
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
			if status >= http.StatusBadRequest && outcome.name == "" {
				reason := wrapped.errorCode
				if reason == "" {
					reason = fmt.Sprintf("http_%d", status)
				}
				outcome.name = "request_failed"
				outcome.generic = true
				outcome.attributes = append(outcome.attributes, "reason", reason)
			}
			event := "http_request_completed"
			if outcome.name != "" && !outcome.generic {
				event = outcome.name
			}
			attributes := []any{
				"event", event,
				"requestId", requestID,
				"traceId", traceContext.TraceID,
				"spanId", traceContext.SpanID,
				"method", request.Method,
				"route", RoutePattern(request),
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
			if wrapped.errorCode != "" {
				attributes = append(attributes, "errorCode", wrapped.errorCode)
			}
			if outcome.err != "" {
				attributes = append(attributes, "error", outcome.err)
			}
			if status >= http.StatusBadRequest {
				eventAttributes := []attribute.KeyValue{attribute.Int("http.response.status_code", status)}
				if wrapped.errorCode != "" {
					eventAttributes = append(eventAttributes, attribute.String("error.code", wrapped.errorCode))
				}
				oteltrace.SpanFromContext(request.Context()).AddEvent(
					"http.response.error",
					oteltrace.WithAttributes(eventAttributes...),
				)
			}
			level := slog.LevelInfo
			if status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			if level == slog.LevelInfo && status < http.StatusBadRequest && quietRoute(RoutePattern(request)) {
				return
			}
			logger.Log(request.Context(), level, "http request completed", attributes...)
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
		case float64:
			spanAttributes = append(spanAttributes, attribute.Float64(key, value))
		case []string:
			spanAttributes = append(spanAttributes, attribute.StringSlice(key, value))
		default:
			spanAttributes = append(spanAttributes, attribute.String(key, fmt.Sprint(value)))
		}
	}
	span := oteltrace.SpanFromContext(request.Context())
	span.AddEvent(name, oteltrace.WithAttributes(spanAttributes...))
	span.SetAttributes(attribute.String("app.outcome", name))
	for _, item := range spanAttributes {
		span.SetAttributes(attribute.KeyValue{Key: attribute.Key("app." + snakeCase(string(item.Key))), Value: item.Value})
	}
}

func snakeCase(value string) string {
	var builder strings.Builder
	for index, character := range value {
		if character >= 'A' && character <= 'Z' {
			if index > 0 {
				builder.WriteByte('_')
			}
			builder.WriteRune(character + ('a' - 'A'))
			continue
		}
		builder.WriteRune(character)
	}
	return builder.String()
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
