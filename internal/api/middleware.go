package api

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

// Rate limit defaults from docs/API.md.
const (
	DefaultRateLimit      = 120 // requests per minute per token
	DefaultLoginRateLimit = 5   // login attempts per minute per IP
	rateWindow            = time.Minute
)

// rateLimiter is a fixed-window counter keyed by token or IP.
//
// A fixed window can let through up to twice the limit across a window
// boundary. That is acceptable here: the limit exists to blunt abuse and
// brute force, not to meter billing, and the simpler structure has no
// per-key goroutine to leak.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	buckets map[string]*rateBucket
}

type rateBucket struct {
	count     int
	resetsAt  time.Time
	lastSeenA time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		now:     time.Now,
		buckets: make(map[string]*rateBucket),
	}
}

// allow records a request and reports whether it is within the limit, along
// with the remaining allowance and when the window resets.
func (l *rateLimiter) allow(key string) (ok bool, remaining int, resetsAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	bucket, exists := l.buckets[key]
	if !exists || !now.Before(bucket.resetsAt) {
		bucket = &rateBucket{resetsAt: now.Add(l.window)}
		l.buckets[key] = bucket
	}

	bucket.lastSeenA = now
	bucket.count++

	remaining = l.limit - bucket.count
	if remaining < 0 {
		remaining = 0
	}
	return bucket.count <= l.limit, remaining, bucket.resetsAt
}

// sweepLocked drops buckets whose window has long passed, so the map does not
// grow without bound on a busy or hostile network.
func (l *rateLimiter) sweepLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if now.Sub(bucket.lastSeenA) > 10*l.window {
			delete(l.buckets, key)
		}
	}
}

// rateLimit is middleware applying a limit keyed by the function's result.
func (a *API) rateLimit(limiter *rateLimiter, keyOf func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyOf(r)
			ok, remaining, resetsAt := limiter.allow(key)

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limiter.limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetsAt.Unix(), 10))

			if !ok {
				retryAfter := int(time.Until(resetsAt).Seconds()) + 1
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				writeError(w, http.StatusTooManyRequests, CodeRateLimited,
					"too many requests, retry in "+strconv.Itoa(retryAfter)+"s")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// tokenKey limits by bearer token, falling back to the client address for
// unauthenticated requests so a missing token cannot dodge the limit.
func tokenKey(r *http.Request) string {
	if raw, ok := bearerToken(r); ok {
		// Hashed rather than used directly: the limiter map outlives the
		// request, so raw bearer tokens would sit in long-lived memory for no
		// reason. Only the identity of the token matters here, and a hash
		// carries that just as well.
		return "token:" + store.HashToken(raw)
	}
	return "ip:" + clientIP(r)
}

func ipKey(r *http.Request) string { return "ip:" + clientIP(r) }

// clientIP extracts the caller address. X-Forwarded-For is deliberately not
// trusted: without a configured trusted-proxy list, any client could forge it
// and bypass the login limit entirely.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack delegates to the wrapped writer. gorilla/websocket asserts the
// ResponseWriter to http.Hijacker directly rather than going through
// http.ResponseController, so without this the console upgrade fails with a
// bad handshake as soon as any middleware wraps the writer.
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("api: the underlying writer does not support hijacking")
	}
	// A hijacked connection writes its own status line, so record the upgrade
	// rather than leaving the log line reporting 200.
	w.status = http.StatusSwitchingProtocols
	return hijacker.Hijack()
}

// Flush delegates to the wrapped writer so streaming responses are not held
// back by the wrapper.
func (w *statusRecorder) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// logRequests logs one line per request.
func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		attrs := []any{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Duration("took", time.Since(start)),
			slog.String("ip", clientIP(r)),
		}
		if principal, ok := principalFrom(r.Context()); ok {
			attrs = append(attrs, slog.String("user_id", principal.UserID))
		}

		switch {
		case status >= 500:
			a.log.Error("request failed", attrs...)
		case status >= 400:
			a.log.Warn("request rejected", attrs...)
		default:
			a.log.Info("request", attrs...)
		}
	})
}

// chain applies middleware in the order given, so the first listed is the
// outermost.
func chain(h http.Handler, middleware ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		h = middleware[i](h)
	}
	return h
}
