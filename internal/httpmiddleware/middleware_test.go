package httpmiddleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
