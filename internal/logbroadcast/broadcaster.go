package logbroadcast

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	URL            string
	Username       string
	Password       string
	Environment    string
	QueueBytes     int
	BatchBytes     int
	FlushInterval  time.Duration
	RequestTimeout time.Duration
}

type Broadcaster struct {
	enabled         bool
	url             string
	username        string
	password        string
	environment     string
	maxQueueBytes   int
	maxBatchBytes   int
	flushInterval   time.Duration
	client          *http.Client
	mutex           sync.Mutex
	queue           []entry
	queueBytes      int
	closed          bool
	wake            chan struct{}
	shutdown        chan context.Context
	done            chan struct{}
	closeOnce       sync.Once
	enqueued        atomic.Uint64
	delivered       atomic.Uint64
	queueDropped    atomic.Uint64
	deliveryDropped atomic.Uint64
	deliveryErrors  atomic.Uint64
	currentBytes    atomic.Uint64
	currentEntries  atomic.Uint64
}

type entry struct {
	timestamp int64
	line      string
	size      int
}

type pushPayload struct {
	Streams []pushStream `json:"streams"`
}

type pushStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type responseError struct {
	status    int
	retriable bool
}

func (err responseError) Error() string {
	return fmt.Sprintf("loki returned status %d", err.status)
}

func New(cfg Config) (*Broadcaster, error) {
	broadcaster := &Broadcaster{
		enabled:       cfg.URL != "",
		url:           cfg.URL,
		username:      cfg.Username,
		password:      cfg.Password,
		environment:   cfg.Environment,
		maxQueueBytes: cfg.QueueBytes,
		maxBatchBytes: cfg.BatchBytes,
		flushInterval: cfg.FlushInterval,
		client:        &http.Client{Timeout: cfg.RequestTimeout},
		wake:          make(chan struct{}, 1),
		shutdown:      make(chan context.Context, 1),
		done:          make(chan struct{}),
	}
	if !broadcaster.enabled {
		close(broadcaster.done)
		return broadcaster, nil
	}

	parsed, err := url.ParseRequestURI(cfg.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("LOKI_PUSH_URL must be an absolute HTTP or HTTPS URL")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("LOKI_USERNAME and LOKI_PASSWORD are required when LOKI_PUSH_URL is set")
	}
	if cfg.Environment == "" {
		return nil, errors.New("LOKI_ENVIRONMENT is required when LOKI_PUSH_URL is set")
	}
	if cfg.QueueBytes <= 0 || cfg.BatchBytes <= 0 || cfg.BatchBytes > cfg.QueueBytes {
		return nil, errors.New("Loki queue and batch byte limits are invalid")
	}
	if cfg.FlushInterval <= 0 || cfg.RequestTimeout <= 0 {
		return nil, errors.New("Loki flush interval and request timeout must be positive")
	}

	go broadcaster.run()
	return broadcaster, nil
}

func (broadcaster *Broadcaster) Enabled() bool {
	return broadcaster.enabled
}

func (broadcaster *Broadcaster) Write(data []byte) (int, error) {
	if !broadcaster.enabled {
		return len(data), nil
	}

	line := strings.TrimSuffix(string(data), "\n")
	size := len(line) + 64

	broadcaster.mutex.Lock()
	if broadcaster.closed || size > broadcaster.maxQueueBytes || broadcaster.queueBytes+size > broadcaster.maxQueueBytes {
		broadcaster.mutex.Unlock()
		broadcaster.queueDropped.Add(1)
		return len(data), nil
	}
	broadcaster.queue = append(broadcaster.queue, entry{
		timestamp: time.Now().UnixNano(),
		line:      line,
		size:      size,
	})
	broadcaster.queueBytes += size
	queueBytes := broadcaster.queueBytes
	queueEntries := len(broadcaster.queue)
	broadcaster.currentBytes.Store(uint64(queueBytes))
	broadcaster.currentEntries.Store(uint64(queueEntries))
	broadcaster.mutex.Unlock()

	broadcaster.enqueued.Add(1)
	if queueBytes >= broadcaster.maxBatchBytes {
		select {
		case broadcaster.wake <- struct{}{}:
		default:
		}
	}
	return len(data), nil
}

func (broadcaster *Broadcaster) Close(ctx context.Context) error {
	if !broadcaster.enabled {
		return nil
	}

	broadcaster.closeOnce.Do(func() {
		broadcaster.mutex.Lock()
		broadcaster.closed = true
		broadcaster.mutex.Unlock()
		broadcaster.shutdown <- ctx
	})

	select {
	case <-broadcaster.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (broadcaster *Broadcaster) EnqueuedTotal() uint64 {
	return broadcaster.enqueued.Load()
}

func (broadcaster *Broadcaster) DeliveredTotal() uint64 {
	return broadcaster.delivered.Load()
}

func (broadcaster *Broadcaster) QueueDroppedTotal() uint64 {
	return broadcaster.queueDropped.Load()
}

func (broadcaster *Broadcaster) DeliveryDroppedTotal() uint64 {
	return broadcaster.deliveryDropped.Load()
}

func (broadcaster *Broadcaster) DeliveryErrorsTotal() uint64 {
	return broadcaster.deliveryErrors.Load()
}

func (broadcaster *Broadcaster) QueueBytes() uint64 {
	return broadcaster.currentBytes.Load()
}

func (broadcaster *Broadcaster) QueueEntries() uint64 {
	return broadcaster.currentEntries.Load()
}

func (broadcaster *Broadcaster) run() {
	ticker := time.NewTicker(broadcaster.flushInterval)
	defer ticker.Stop()
	defer close(broadcaster.done)

	for {
		select {
		case <-ticker.C:
			broadcaster.flush(context.Background(), false)
		case <-broadcaster.wake:
			broadcaster.flush(context.Background(), false)
		case ctx := <-broadcaster.shutdown:
			broadcaster.flush(ctx, true)
			return
		}
	}
}

func (broadcaster *Broadcaster) flush(ctx context.Context, all bool) {
	for {
		batch := broadcaster.takeBatch()
		if len(batch) == 0 {
			return
		}
		if err := broadcaster.deliver(ctx, batch); err != nil {
			broadcaster.deliveryErrors.Add(1)
			broadcaster.deliveryDropped.Add(uint64(len(batch)))
		} else {
			broadcaster.delivered.Add(uint64(len(batch)))
		}
		if !all && broadcaster.QueueBytes() < uint64(broadcaster.maxBatchBytes) {
			return
		}
	}
}

func (broadcaster *Broadcaster) takeBatch() []entry {
	broadcaster.mutex.Lock()
	defer broadcaster.mutex.Unlock()

	if len(broadcaster.queue) == 0 {
		return nil
	}

	count := 0
	size := 0
	for count < len(broadcaster.queue) {
		next := broadcaster.queue[count].size
		if count > 0 && size+next > broadcaster.maxBatchBytes {
			break
		}
		size += next
		count++
	}

	batch := make([]entry, count)
	copy(batch, broadcaster.queue[:count])
	for index := 0; index < count; index++ {
		broadcaster.queue[index] = entry{}
	}
	broadcaster.queue = broadcaster.queue[count:]
	broadcaster.queueBytes -= size
	broadcaster.currentBytes.Store(uint64(broadcaster.queueBytes))
	broadcaster.currentEntries.Store(uint64(len(broadcaster.queue)))
	return batch
}

func (broadcaster *Broadcaster) deliver(ctx context.Context, batch []entry) error {
	values := make([][2]string, len(batch))
	for index, item := range batch {
		values[index] = [2]string{fmt.Sprintf("%d", item.timestamp), item.line}
	}
	payload := pushPayload{
		Streams: []pushStream{{
			Stream: map[string]string{
				"environment":  broadcaster.environment,
				"service_name": "seat-reservation",
				"source":       "application",
			},
			Values: values,
		}},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(raw); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt*attempt) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodPost, broadcaster.url, bytes.NewReader(compressed.Bytes()))
		if err != nil {
			return err
		}
		request.SetBasicAuth(broadcaster.username, broadcaster.password)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Content-Encoding", "gzip")

		response, err := broadcaster.client.Do(request)
		if err == nil {
			_, copyErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if copyErr != nil {
				err = copyErr
			} else if closeErr != nil {
				err = closeErr
			} else if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			} else {
				err = responseError{
					status:    response.StatusCode,
					retriable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500,
				}
			}
		}
		lastErr = err

		var statusErr responseError
		if errors.As(err, &statusErr) && !statusErr.retriable {
			break
		}
	}
	return lastErr
}
