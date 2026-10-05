package httpmiddleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"seat-reservation/internal/telemetry"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRequestCorrelationPropagation(t *testing.T) {
	const requestID = "checkout-42"
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	handler := Trace(RequestID(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := RequestIDFromContext(request.Context()); got != requestID {
			t.Fatalf("request ID = %q, want %q", got, requestID)
		}
		traceContext := TraceFromContext(request.Context())
		if traceContext.TraceID != traceID {
			t.Fatalf("trace ID = %q, want %q", traceContext.TraceID, traceID)
		}
		if len(traceContext.SpanID) != 16 {
			t.Fatalf("span ID length = %d, want 16", len(traceContext.SpanID))
		}
		writer.WriteHeader(http.StatusNoContent)
	})))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Request-ID", requestID)
	request.Header.Set("Traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("X-Request-ID"); got != requestID {
		t.Fatalf("response request ID = %q, want %q", got, requestID)
	}
	if got := response.Header().Get("X-Trace-ID"); got != traceID {
		t.Fatalf("response trace ID = %q, want %q", got, traceID)
	}
	if got := response.Header().Get("Traceparent"); !strings.HasPrefix(got, "00-"+traceID+"-") {
		t.Fatalf("response traceparent = %q", got)
	}
}

func TestInvalidCorrelationValuesAreReplaced(t *testing.T) {
	handler := Trace(RequestID(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := RequestIDFromContext(request.Context()); got == "invalid request id" {
			t.Fatal("invalid request ID was preserved")
		}
		if got := TraceFromContext(request.Context()).TraceID; got == strings.Repeat("0", 32) {
			t.Fatal("invalid trace ID was preserved")
		}
		writer.WriteHeader(http.StatusNoContent)
	})))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Request-ID", "invalid request id")
	request.Header.Set("Traceparent", "00-"+strings.Repeat("0", 32)+"-00f067aa0ba902b7-01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
}

func TestTraceRecordsCompleteServerSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previous)

	handler := Trace(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		traceContext := TraceFromContext(request.Context())
		if traceContext.TraceID == "" || traceContext.SpanID == "" {
			t.Fatal("trace context was not attached to request")
		}
		writer.WriteHeader(http.StatusCreated)
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/shows/42/reserve", nil))

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "POST /shows/42/reserve" {
		t.Fatalf("span name = %q", span.Name())
	}
	if !span.EndTime().After(span.StartTime()) {
		t.Fatal("span does not cover request duration")
	}
	if got := response.Header().Get("X-Trace-ID"); got != span.SpanContext().TraceID().String() {
		t.Fatalf("response trace ID = %q, span trace ID = %q", got, span.SpanContext().TraceID())
	}
}

type routeObserver struct {
	routes []string
}

func (observer *routeObserver) ObserveRequest(_ string, route string, _ int, _ time.Duration) {
	observer.routes = append(observer.routes, route)
}

func newObservedStack(logger *slog.Logger, observer RequestObserver, router http.Handler) http.Handler {
	handler := Recover(logger)(CaptureRoute(router))
	handler = AccessLog(logger)(handler)
	handler = Metrics(observer)(handler)
	handler = Trace(handler)
	return RequestID(handler)
}

func TestRoutePatternSurvivesRequestCopies(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previous)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	observer := &routeObserver{}
	router := http.NewServeMux()
	router.HandleFunc("GET /shows/{id}", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})

	newObservedStack(logger, observer, router).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/shows/abc", nil),
	)

	if len(observer.routes) != 1 || observer.routes[0] != "GET /shows/{id}" {
		t.Fatalf("metrics routes = %q", observer.routes)
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "GET /shows/{id}" {
		t.Fatalf("spans = %v", spans)
	}
	if !strings.Contains(logs.String(), `"route":"GET /shows/{id}"`) {
		t.Fatalf("access log route missing: %s", logs.String())
	}
}

func TestAccessLogSkipsHealthyQuietRoutesAndEscalatesServerErrors(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	router := http.NewServeMux()
	router.HandleFunc("GET /health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	router.HandleFunc("GET /health/ready", func(writer http.ResponseWriter, _ *http.Request) {
		writeError(writer, http.StatusServiceUnavailable, "not_ready")
	})
	stack := newObservedStack(logger, &routeObserver{}, router)

	stack.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if logs.Len() != 0 {
		t.Fatalf("healthy liveness probe was logged: %s", logs.String())
	}

	stack.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	line := logs.String()
	for _, want := range []string{`"level":"ERROR"`, `"status":503`, `"errorCode":"not_ready"`, `"reason":"not_ready"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %s: %s", want, line)
		}
	}
}

func TestReservationOutcomeBecomesEvent(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	router := http.NewServeMux()
	router.HandleFunc("POST /shows/{id}/reserve", func(writer http.ResponseWriter, request *http.Request) {
		RecordRequestOutcome(request, "reservation_declined", "reason", "seats_unavailable", "seatCount", 2, "ratio", 0.5)
		writeError(writer, http.StatusConflict, "seats_unavailable")
	})

	newObservedStack(logger, &routeObserver{}, router).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/shows/abc/reserve", nil),
	)

	line := logs.String()
	for _, want := range []string{`"event":"reservation_declined"`, `"outcome":"reservation_declined"`, `"reason":"seats_unavailable"`, `"level":"INFO"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %s: %s", want, line)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("expected exactly one log line: %s", line)
	}
}

func TestRequestSpansAreLoggedAsOrderedTree(t *testing.T) {
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(telemetry.DefaultCollector),
	)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previous)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	router := http.NewServeMux()
	router.HandleFunc("POST /shows/{id}/reserve", func(writer http.ResponseWriter, request *http.Request) {
		ctx, reserve := telemetry.Start(request.Context(), "reservation.reserve")
		stepContext, step := telemetry.Start(ctx, "reservation.lock_seats")
		_, query := telemetry.Start(stepContext, "db SELECT")
		query.End()
		telemetry.Finish(step, errors.New("seats taken"), errors.New("seats taken"))
		telemetry.Finish(reserve, nil)
		RecordRequestOutcome(request, "reservation_declined", "reason", "seats_unavailable")
		writeError(writer, http.StatusConflict, "seats_unavailable")
	})

	newObservedStack(logger, &routeObserver{}, router).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/shows/abc/reserve", nil),
	)

	type spanLine struct {
		Event string `json:"event"`
		Name  string `json:"name"`
		Depth int    `json:"depth"`
		Index int    `json:"spanIndex"`
	}
	var spans []spanLine
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var parsed spanLine
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		if parsed.Event == "span" {
			spans = append(spans, parsed)
		}
	}
	if len(spans) != 4 {
		t.Fatalf("span lines = %d, want 4: %s", len(spans), logs.String())
	}
	want := []struct {
		name  string
		depth int
	}{{"db SELECT", 3}, {"reservation.lock_seats", 2}, {"reservation.reserve", 1}, {"POST /shows/{id}/reserve", 0}}
	for index, expected := range want {
		if spans[index].Name != expected.name || spans[index].Depth != expected.depth {
			t.Fatalf("span %d = %+v, want %v", index, spans[index], expected)
		}
	}
	if spans[3].Index != 0 {
		t.Fatalf("root span index = %d", spans[3].Index)
	}
}
