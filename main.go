package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
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

	// Initialize SQLite Database
	dbPath := os.Getenv("PROXY_LOGS_DB")
	if dbPath == "" {
		dbPath = "proxy_logs.db"
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("FATAL: Failed to open SQLite DB: %v", err)
	}
	defer db.Close()

	// Create table if not exists
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS api_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TEXT NOT NULL,
			method TEXT NOT NULL,
			path TEXT NOT NULL,
			request_headers TEXT,
			request_body TEXT,
			response_status INTEGER,
			response_headers TEXT,
			response_body TEXT,
			error TEXT,
			duration_ms INTEGER
		);
	`)
	if err != nil {
		log.Fatalf("FATAL: Failed to create api_logs table: %v", err)
	}

	go startLogWorker(db)
	log.Printf("📂 SQLite Logging enabled. File: %s", dbPath)

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

	// Intercept and modify the response before returning to client
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Format response headers
		var respHeaders []string
		for k, v := range resp.Header {
			respHeaders = append(respHeaders, k+": "+strings.Join(v, ", "))
		}
		respHeadersStr := strings.Join(respHeaders, "\n")

		// Retrieve request details from context
		if details, ok := resp.Request.Context().Value(requestDetailsKey).(*RequestDetails); ok {
			// Wrap resp.Body so we log asynchronously when reading completes or closes
			resp.Body = &loggingReader{
				ReadCloser: resp.Body,
				onClose: func(respBodyBytes []byte) {
					duration := time.Since(details.StartTime)
					logChan <- &LogEntry{
						Timestamp:       details.StartTime,
						Method:          details.Method,
						Path:            details.Path,
						RequestHeaders:  details.Headers,
						RequestBody:     details.Body,
						ResponseStatus:  resp.StatusCode,
						ResponseHeaders: respHeadersStr,
						ResponseBody:    string(respBodyBytes),
						Error:           "",
						DurationMs:      duration.Milliseconds(),
					}
				},
			}
		}

		return nil
	}

	// Capture errors (e.g. backend down)
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		log.Printf("[Proxy] ❌ Target error: %v", err)

		if details, ok := req.Context().Value(requestDetailsKey).(*RequestDetails); ok {
			duration := time.Since(details.StartTime)
			logChan <- &LogEntry{
				Timestamp:       details.StartTime,
				Method:          details.Method,
				Path:            details.Path,
				RequestHeaders:  details.Headers,
				RequestBody:     details.Body,
				ResponseStatus:  http.StatusBadGateway,
				ResponseHeaders: "",
				ResponseBody:    "",
				Error:           err.Error(),
				DurationMs:      duration.Milliseconds(),
			}
		}

		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	// 4. Rate Limiter configuration
	rpm := getEnvInt("RATE_LIMIT_RPM", 0) // Default to 0 (disabled)
	burst := getEnvInt("RATE_LIMIT_BURST", 0)

	// 5. Start the server
	port := os.Getenv("PROXY_PORT")
	if port == "" {
		port = ":8318"
	} else if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	log.Printf("🚀 LLM API Proxy running on http://localhost%s", port)
	log.Printf("➡️ Proxy Target URL: %s", targetURL)

	var handler http.Handler
	if rpm > 0 {
		if burst <= 0 {
			burst = 1 // Ensure we have at least 1 capacity if burst is omitted
		}
		refillRate := float64(rpm) / 60.0
		limiter := NewRateLimiter(float64(burst), refillRate)
		handler = loggingMiddleware(rateLimitMiddleware(limiter, proxy))
		log.Printf("➡️ Rate Limiting: ENABLED (%d RPM, Burst: %d)", rpm, burst)
	} else {
		handler = loggingMiddleware(proxy)
		log.Printf("➡️ Rate Limiting: DISABLED")
	}

	if len(headersToInject) > 0 {
		log.Printf("🔑 Injected headers:")
		for k := range headersToInject {
			log.Printf("   - %s", k)
		}
	} else {
		log.Printf("ℹ️ No headers configured to inject.")
	}

	if err := http.ListenAndServe(port, handler); err != nil {
		log.Fatal("Server error:", err)
	}
}

type LogEntry struct {
	Timestamp       time.Time
	Method          string
	Path            string
	RequestHeaders  string
	RequestBody     string
	ResponseStatus  int
	ResponseHeaders string
	ResponseBody    string
	Error           string
	DurationMs      int64
}

var logChan = make(chan *LogEntry, 1000)

func startLogWorker(db *sql.DB) {
	for entry := range logChan {
		_, err := db.Exec(`
			INSERT INTO api_logs (
				timestamp, method, path, request_headers, request_body,
				response_status, response_headers, response_body, error, duration_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			entry.Timestamp.Format(time.RFC3339),
			entry.Method,
			entry.Path,
			entry.RequestHeaders,
			entry.RequestBody,
			entry.ResponseStatus,
			entry.ResponseHeaders,
			entry.ResponseBody,
			entry.Error,
			entry.DurationMs,
		)
		if err != nil {
			log.Printf("[Logger] ❌ Error writing log to DB: %v", err)
		}
	}
}

type contextKey string

const requestDetailsKey contextKey = "requestDetails"

type RequestDetails struct {
	StartTime time.Time
	Method    string
	Path      string
	Headers   string
	Body      string
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		startTime := time.Now()

		// Capture request headers
		var reqHeaders []string
		for k, v := range req.Header {
			reqHeaders = append(reqHeaders, k+": "+strings.Join(v, ", "))
		}
		reqHeadersStr := strings.Join(reqHeaders, "\n")

		// Capture request body (buffer it)
		var bodyBytes []byte
		if req.Body != nil {
			var err error
			bodyBytes, err = io.ReadAll(req.Body)
			if err != nil {
				log.Printf("[Logger] Error reading request body: %v", err)
			}
			req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		details := &RequestDetails{
			StartTime: startTime,
			Method:    req.Method,
			Path:      req.URL.Path,
			Headers:   reqHeadersStr,
			Body:      string(bodyBytes),
		}

		ctx := context.WithValue(req.Context(), requestDetailsKey, details)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

type loggingReader struct {
	io.ReadCloser
	buf       bytes.Buffer
	onClose   func([]byte)
	closeOnce sync.Once
}

func (lr *loggingReader) Read(p []byte) (n int, err error) {
	n, err = lr.ReadCloser.Read(p)
	if n > 0 {
		lr.buf.Write(p[:n])
	}
	if err == io.EOF {
		lr.closeOnce.Do(func() {
			lr.onClose(lr.buf.Bytes())
		})
	}
	return n, err
}

func (lr *loggingReader) Close() error {
	err := lr.ReadCloser.Close()
	lr.closeOnce.Do(func() {
		lr.onClose(lr.buf.Bytes())
	})
	return err
}
