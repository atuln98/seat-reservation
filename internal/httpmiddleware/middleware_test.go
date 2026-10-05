package httpmiddleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
