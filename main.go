package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimiter implements a thread-safe token-bucket rate limiter with queue reservation.
type RateLimiter struct {
	mu           sync.Mutex
	capacity     float64
	tokens       float64
	refillRate   float64 // tokens per second
	lastRefilled time.Time
	nextFree     time.Time
}

// NewRateLimiter creates a new RateLimiter.
func NewRateLimiter(capacity, refillRate float64) *RateLimiter {
	now := time.Now()
	return &RateLimiter{
		capacity:     capacity,
		tokens:       capacity,
		refillRate:   refillRate,
		lastRefilled: now,
		nextFree:     now,
	}
}

// Wait blocks until a token is available or the context is cancelled.
func (rl *RateLimiter) Wait(ctx context.Context) error {
	rl.mu.Lock()
	now := time.Now()

	// 1. Refill tokens
	elapsed := now.Sub(rl.lastRefilled).Seconds()
	rl.tokens += elapsed * rl.refillRate
	if rl.tokens > rl.capacity {
		rl.tokens = rl.capacity
	}
	rl.lastRefilled = now

	// 2. If nextFree is in the past, align it with now
	if rl.nextFree.Before(now) {
		rl.nextFree = now
	}

	// 3. If we have at least 1.0 token and no queue is active, consume it immediately
	if rl.tokens >= 1.0 && rl.nextFree.Equal(now) {
		rl.tokens -= 1.0
		rl.mu.Unlock()
		return nil
	}

	// 4. Otherwise, queue/reserve a slot
	var readyTime time.Time
	if rl.nextFree.After(now) {
		readyTime = rl.nextFree.Add(time.Duration((1.0 / rl.refillRate) * float64(time.Second)))
	} else {
		needed := 1.0 - rl.tokens
		readyTime = now.Add(time.Duration((needed / rl.refillRate) * float64(time.Second)))
	}

	sleepTime := readyTime.Sub(now)
	rl.nextFree = readyTime
	rl.tokens -= 1.0
	rl.mu.Unlock()

	if sleepTime <= 0 {
		return nil
	}

	log.Printf("[RateLimiter] ⏳ Queueing request. Estimated wait time: %.2fs", sleepTime.Seconds())

	timer := time.NewTimer(sleepTime)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		rl.refund()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// refund restores one token to the bucket and adjusts the queue timing if a request is cancelled.
func (rl *RateLimiter) refund() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.tokens += 1.0
	if rl.tokens > rl.capacity {
		rl.tokens = rl.capacity
	}

	costDuration := time.Duration((1.0 / rl.refillRate) * float64(time.Second))
	if rl.nextFree.After(time.Now()) {
		rl.nextFree = rl.nextFree.Add(-costDuration)
		if rl.nextFree.Before(time.Now()) {
			rl.nextFree = time.Now()
		}
	}
	log.Printf("[RateLimiter] ↩️ Request cancelled. Token refunded and queue adjusted.")
}

func getEnvInt(key string, defaultVal int) int {
	if val, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func rateLimitMiddleware(limiter *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		log.Printf("[Proxy] 📥 Incoming request: %s %s", req.Method, req.URL.Path)

		err := limiter.Wait(req.Context())
		if err != nil {
			log.Printf("[Proxy] ❌ Request %s %s cancelled/aborted in queue: %v", req.Method, req.URL.Path, err)
			http.Error(w, "Request cancelled or timed out in queue", http.StatusGatewayTimeout)
			return
		}

		log.Printf("[Proxy] 📤 Forwarding request: %s %s", req.Method, req.URL.Path)
		next.ServeHTTP(w, req)
	})
}

func main() {
	// 1. Load configuration
	targetURL := os.Getenv("PROXY_TARGET_URL")
	if targetURL == "" {
		log.Fatal("FATAL: PROXY_TARGET_URL environment variable is required.")
	}

	target, err := url.Parse(targetURL)
	if err != nil {
		log.Fatalf("FATAL: Failed to parse PROXY_TARGET_URL '%s': %v", targetURL, err)
	}

	// 2. Parse headers to inject
	headersToInject := make(map[string]string)

	// A. Parse from environment variables starting with HEADER_
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		if strings.HasPrefix(key, "HEADER_") {
			headerName := strings.TrimPrefix(key, "HEADER_")
			headerName = strings.ReplaceAll(headerName, "_", "-")
			canonicalName := http.CanonicalHeaderKey(headerName)
			headersToInject[canonicalName] = val
		}
	}

	// B. Parse from structured JSON if provided
	headersJSON := os.Getenv("INJECT_HEADERS_JSON")
	if headersJSON != "" {
		var jsonHeaders map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &jsonHeaders); err != nil {
			log.Fatalf("FATAL: Failed to parse INJECT_HEADERS_JSON: %v", err)
		}
		for k, v := range jsonHeaders {
			headersToInject[http.CanonicalHeaderKey(k)] = v
		}
	}

	// 3. Create reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Intercept and modify the request before it goes out
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)

		// Override Host header for proper SSL routing at the destination
		req.Host = target.Host

		// Inject headers
		for k, v := range headersToInject {
			req.Header.Set(k, v)
		}
	}

	// 4. Rate Limiter configuration
	rpm := getEnvInt("RATE_LIMIT_RPM", 20)
	burst := getEnvInt("RATE_LIMIT_BURST", 5)

	refillRate := float64(rpm) / 60.0
	limiter := NewRateLimiter(float64(burst), refillRate)

	// 5. Start the server
	port := os.Getenv("PROXY_PORT")
	if port == "" {
		port = ":8318"
	} else if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	log.Printf("🚀 Rate-Limiting Proxy running on http://localhost%s", port)
	log.Printf("➡️ Proxy Target URL: %s", targetURL)
	log.Printf("➡️ Rate Limit: %d RPM, Burst: %d", rpm, burst)
	if len(headersToInject) > 0 {
		log.Printf("🔑 Injected headers:")
		for k := range headersToInject {
			log.Printf("   - %s", k)
		}
	} else {
		log.Printf("ℹ️ No headers configured to inject.")
	}

	handler := rateLimitMiddleware(limiter, proxy)
	if err := http.ListenAndServe(port, handler); err != nil {
		log.Fatal("Server error:", err)
	}
}
