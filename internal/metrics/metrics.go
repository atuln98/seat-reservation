package metrics

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry              *prometheus.Registry
	reservationsConfirmed prometheus.Counter
	reservationsDeclined  *prometheus.CounterVec
	idempotentReplays     prometheus.Counter
	httpRequests          *prometheus.CounterVec
	httpDuration          *prometheus.HistogramVec
	collectionErrors      prometheus.Counter
}

type showSeatCounts struct {
	showID    string
	available int64
	held      int64
	confirmed int64
	total     int64
}

type seatCollector struct {
	database         *pgxpool.Pool
	collectionErrors prometheus.Counter
	available        *prometheus.Desc
	held             *prometheus.Desc
	confirmed        *prometheus.Desc
	total            *prometheus.Desc
	mutex            sync.Mutex
	refreshedAt      time.Time
	counts           []showSeatCounts
}

func New(database *pgxpool.Pool) *Metrics {
	registry := prometheus.NewRegistry()
	metrics := &Metrics{
		registry: registry,
		reservationsConfirmed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "seat_reservation",
			Name:      "reservations_confirmed_total",
			Help:      "Reservations confirmed by this service instance.",
		}),
		reservationsDeclined: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "seat_reservation",
			Name:      "reservations_declined_total",
			Help:      "Reservation requests declined by reason.",
		}, []string{"reason"}),
		idempotentReplays: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "seat_reservation",
			Name:      "idempotent_replays_total",
			Help:      "Reservation requests served from an existing idempotency record.",
		}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "seat_reservation",
			Name:      "http_requests_total",
			Help:      "HTTP requests by method, route, and status.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "seat_reservation",
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request duration by method and route.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "route"}),
		collectionErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "seat_reservation",
			Name:      "metrics_collection_errors_total",
			Help:      "Failures while collecting database-backed metrics.",
		}),
	}

	seats := newSeatCollector(database, metrics.collectionErrors)

	registry.MustRegister(
		metrics.reservationsConfirmed,
		metrics.reservationsDeclined,
		metrics.idempotentReplays,
		metrics.httpRequests,
		metrics.httpDuration,
		metrics.collectionErrors,
		seats,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	for _, reason := range []string{"seat_taken", "per_user_limit", "idempotent_replay", "idempotency_conflict"} {
		metrics.reservationsDeclined.WithLabelValues(reason).Add(0)
	}
	return metrics
}

func (metrics *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{
		EnableOpenMetrics:   true,
		MaxRequestsInFlight: 2,
		Timeout:             3 * time.Second,
	})
}

func (metrics *Metrics) Confirmed() {
	metrics.reservationsConfirmed.Inc()
}

func (metrics *Metrics) Declined(reason string) {
	metrics.reservationsDeclined.WithLabelValues(reason).Inc()
}

func (metrics *Metrics) Replayed() {
	metrics.idempotentReplays.Inc()
	metrics.reservationsDeclined.WithLabelValues("idempotent_replay").Inc()
}

func (metrics *Metrics) ObserveRequest(method string, route string, status int, duration time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	metrics.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	metrics.httpDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

func newSeatCollector(database *pgxpool.Pool, collectionErrors prometheus.Counter) *seatCollector {
	labels := []string{"show_id"}
	return &seatCollector{
		database:         database,
		collectionErrors: collectionErrors,
		available: prometheus.NewDesc(
			"seat_reservation_seats_available",
			"Current available seats for a show.",
			labels,
			nil,
		),
		held: prometheus.NewDesc(
			"seat_reservation_seats_held",
			"Current held seats for a show.",
			labels,
			nil,
		),
		confirmed: prometheus.NewDesc(
			"seat_reservation_seats_confirmed",
			"Current confirmed seats for a show.",
			labels,
			nil,
		),
		total: prometheus.NewDesc(
			"seat_reservation_seats_total",
			"Current total seats for a show.",
			labels,
			nil,
		),
	}
}

func (collector *seatCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- collector.available
	descriptions <- collector.held
	descriptions <- collector.confirmed
	descriptions <- collector.total
}

func (collector *seatCollector) Collect(metrics chan<- prometheus.Metric) {
	for _, count := range collector.snapshot() {
		metrics <- prometheus.MustNewConstMetric(
			collector.available,
			prometheus.GaugeValue,
			float64(count.available),
			count.showID,
		)
		metrics <- prometheus.MustNewConstMetric(
			collector.held,
			prometheus.GaugeValue,
			float64(count.held),
			count.showID,
		)
		metrics <- prometheus.MustNewConstMetric(
			collector.confirmed,
			prometheus.GaugeValue,
			float64(count.confirmed),
			count.showID,
		)
		metrics <- prometheus.MustNewConstMetric(
			collector.total,
			prometheus.GaugeValue,
			float64(count.total),
			count.showID,
		)
	}
}

func (collector *seatCollector) snapshot() []showSeatCounts {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()

	if len(collector.counts) != 0 && time.Since(collector.refreshedAt) < 5*time.Second {
		return append([]showSeatCounts(nil), collector.counts...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	rows, err := collector.database.Query(ctx, `
		SELECT
			show_id::text,
			count(*) FILTER (WHERE state = 'available'),
			count(*) FILTER (WHERE state = 'held'),
			count(*) FILTER (WHERE state = 'confirmed'),
			count(*)
		FROM seats
		GROUP BY show_id
		ORDER BY show_id
	`)
	if err != nil {
		collector.collectionErrors.Inc()
		return append([]showSeatCounts(nil), collector.counts...)
	}
	defer rows.Close()

	counts := make([]showSeatCounts, 0)
	for rows.Next() {
		var count showSeatCounts
		if err := rows.Scan(
			&count.showID,
			&count.available,
			&count.held,
			&count.confirmed,
			&count.total,
		); err != nil {
			collector.collectionErrors.Inc()
			return append([]showSeatCounts(nil), collector.counts...)
		}
		counts = append(counts, count)
	}
	if rows.Err() != nil {
		collector.collectionErrors.Inc()
		return append([]showSeatCounts(nil), collector.counts...)
	}

	collector.counts = counts
	collector.refreshedAt = time.Now()
	return append([]showSeatCounts(nil), counts...)
}
