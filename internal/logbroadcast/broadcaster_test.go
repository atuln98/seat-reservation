package logbroadcast

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBroadcasterDeliversCompressedBatch(t *testing.T) {
	payloads := make(chan pushPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "tenant" || password != "token" {
			t.Errorf("unexpected basic authentication")
		}
		if request.URL.Path != "/loki/api/v1/push" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("Content-Encoding = %q", request.Header.Get("Content-Encoding"))
		}
		reader, err := gzip.NewReader(request.Body)
		if err != nil {
			t.Errorf("create gzip reader: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		defer reader.Close()
		var payload pushPayload
		if err := json.NewDecoder(reader).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		payloads <- payload
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	broadcaster := newTestBroadcaster(t, server.URL+"/loki/api/v1/push", 1024, 1024)
	for _, line := range []string{`{"event":"first"}`, `{"event":"second"}`} {
		if _, err := broadcaster.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	closeBroadcaster(t, broadcaster)

	select {
	case payload := <-payloads:
		if len(payload.Streams) != 1 {
			t.Fatalf("streams = %d", len(payload.Streams))
		}
		stream := payload.Streams[0]
		if stream.Stream["service_name"] != "seat-reservation" || stream.Stream["environment"] != "test" {
			t.Fatalf("stream labels = %#v", stream.Stream)
		}
		if len(stream.Values) != 2 {
			t.Fatalf("values = %d", len(stream.Values))
		}
		if stream.Values[0][1] != `{"event":"first"}` || stream.Values[1][1] != `{"event":"second"}` {
			t.Fatalf("values = %#v", stream.Values)
		}
	case <-time.After(time.Second):
		t.Fatal("Loki payload was not delivered")
	}

	if broadcaster.EnqueuedTotal() != 2 || broadcaster.DeliveredTotal() != 2 {
		t.Fatalf("enqueued = %d, delivered = %d", broadcaster.EnqueuedTotal(), broadcaster.DeliveredTotal())
	}
	if broadcaster.QueueBytes() != 0 || broadcaster.QueueEntries() != 0 {
		t.Fatalf("queue bytes = %d, entries = %d", broadcaster.QueueBytes(), broadcaster.QueueEntries())
	}
}

func TestBroadcasterBoundsQueueMemory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	broadcaster := newTestBroadcaster(t, server.URL, 128, 128)
	if _, err := broadcaster.Write([]byte(`{"event":"first"}`)); err != nil {
		t.Fatalf("first Write() error = %v", err)
	}
	if _, err := broadcaster.Write([]byte(`{"event":"second"}`)); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}

	if broadcaster.QueueBytes() > 128 {
		t.Fatalf("queue bytes = %d", broadcaster.QueueBytes())
	}
	if broadcaster.QueueDroppedTotal() != 1 {
		t.Fatalf("queue drops = %d", broadcaster.QueueDroppedTotal())
	}
	closeBroadcaster(t, broadcaster)
}

func TestBroadcasterDropsAfterDeliveryRetries(t *testing.T) {
	var requests atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	broadcaster := newTestBroadcaster(t, server.URL, 1024, 1024)
	if _, err := broadcaster.Write([]byte(`{"event":"failed"}`)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	closeBroadcaster(t, broadcaster)

	if requests.Load() != 3 {
		t.Fatalf("requests = %d", requests.Load())
	}
	if broadcaster.DeliveryErrorsTotal() != 1 || broadcaster.DeliveryDroppedTotal() != 1 {
		t.Fatalf(
			"delivery errors = %d, delivery drops = %d",
			broadcaster.DeliveryErrorsTotal(),
			broadcaster.DeliveryDroppedTotal(),
		)
	}
}

func newTestBroadcaster(t *testing.T, endpoint string, queueBytes int, batchBytes int) *Broadcaster {
	t.Helper()
	broadcaster, err := New(Config{
		URL:            endpoint,
		Username:       "tenant",
		Password:       "token",
		Environment:    "test",
		QueueBytes:     queueBytes,
		BatchBytes:     batchBytes,
		FlushInterval:  time.Hour,
		RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return broadcaster
}

func closeBroadcaster(t *testing.T, broadcaster *Broadcaster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := broadcaster.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestBroadcasterSeparatesStreamsByLevel(t *testing.T) {
	payloads := make(chan pushPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		reader, err := gzip.NewReader(request.Body)
		if err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		defer reader.Close()
		var payload pushPayload
		if err := json.NewDecoder(reader).Decode(&payload); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		payloads <- payload
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	broadcaster := newTestBroadcaster(t, server.URL+"/loki/api/v1/push", 4096, 4096)
	for _, line := range []string{
		`{"level":"INFO","message":"a"}`,
		`{"level":"ERROR","message":"b"}`,
		`{"level":"INFO","message":"c"}`,
	} {
		if _, err := broadcaster.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	closeBroadcaster(t, broadcaster)

	payload := <-payloads
	counts := map[string]int{}
	for _, stream := range payload.Streams {
		counts[stream.Stream["level"]] += len(stream.Values)
	}
	if len(payload.Streams) != 2 || counts["info"] != 2 || counts["error"] != 1 {
		t.Fatalf("streams = %+v counts = %v", payload.Streams, counts)
	}
}

func TestRemoteLokiEndpointRequiresHTTPS(t *testing.T) {
	_, err := New(Config{
		URL:            "http://logs.example.com/loki/api/v1/push",
		Username:       "tenant",
		Password:       "token",
		Environment:    "test",
		QueueBytes:     1024,
		BatchBytes:     512,
		FlushInterval:  time.Second,
		RequestTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("plain HTTP remote endpoint was accepted")
	}
}
