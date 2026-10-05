package httpmiddleware

import (
	"net"
	"net/http"
	"sync"
	"time"
)

type clientBucket struct {
	tokens    float64
	updatedAt time.Time
}

type RateLimiter struct {
	mutex         sync.Mutex
	ratePerSecond float64
	burst         float64
	clients       map[string]clientBucket
	lastSweep     time.Time
}

func NewRateLimiter(ratePerSecond int, burst int) *RateLimiter {
	return &RateLimiter{
		ratePerSecond: float64(ratePerSecond),
		burst:         float64(burst),
		clients:       make(map[string]clientBucket),
		lastSweep:     time.Now(),
	}
}

func (limiter *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !limiter.allow(clientIP(request), time.Now()) {
			writer.Header().Set("Retry-After", "1")
			writeError(writer, http.StatusTooManyRequests, "rate_limit_exceeded")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (limiter *RateLimiter) allow(client string, now time.Time) bool {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	if now.Sub(limiter.lastSweep) >= time.Minute {
		for key, bucket := range limiter.clients {
			if now.Sub(bucket.updatedAt) >= 10*time.Minute {
				delete(limiter.clients, key)
			}
		}
		limiter.lastSweep = now
	}

	bucket, exists := limiter.clients[client]
	if !exists {
		bucket = clientBucket{tokens: limiter.burst, updatedAt: now}
	}
	bucket.tokens = min(
		limiter.burst,
		bucket.tokens+now.Sub(bucket.updatedAt).Seconds()*limiter.ratePerSecond,
	)
	bucket.updatedAt = now
	if bucket.tokens < 1 {
		limiter.clients[client] = bucket
		return false
	}
	bucket.tokens--
	limiter.clients[client] = bucket
	return true
}

func clientIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}
