package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// loggingMiddleware logs request details: method, path, provider, status, and duration.
type loggingMiddleware struct {
	next http.Handler
}

// responseRecorder captures the status code written by the next handler.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rr *responseRecorder) WriteHeader(code int) {
	if !rr.written {
		rr.statusCode = code
		rr.written = true
	}
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if !rr.written {
		rr.statusCode = http.StatusOK
		rr.written = true
	}
	return rr.ResponseWriter.Write(b)
}

// Flush implements http.Flusher for streaming support.
func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (lm *loggingMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}

	lm.next.ServeHTTP(rec, r)

	duration := time.Since(start)
	log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.statusCode, duration.Round(time.Millisecond))
}

// corsMiddleware handles CORS preflight and adds permissive CORS headers.
type corsMiddleware struct {
	next http.Handler
}

func (cm *corsMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Max-Age", "86400")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	cm.next.ServeHTTP(w, r)
}

// bodySizeLimitMiddleware limits request body size.
type bodySizeLimitMiddleware struct {
	next    http.Handler
	maxSize int64 // 0 means no limit
}

func (bm *bodySizeLimitMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if bm.maxSize > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, bm.maxSize)
	}
	bm.next.ServeHTTP(w, r)
}

// recoveryMiddleware catches panics and returns a 500 response.
type recoveryMiddleware struct {
	next http.Handler
}

func (rm *recoveryMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("PANIC: %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}()
	rm.next.ServeHTTP(w, r)
}

// buildMiddlewareChain wraps the handler with all middleware layers.
// Order (outermost to innermost): Recovery → Logging → CORS → BodySizeLimit → handler
func buildMiddlewareChain(handler http.Handler, maxBodySize int64) http.Handler {
	var h http.Handler = handler

	h = &bodySizeLimitMiddleware{next: h, maxSize: maxBodySize}
	h = &corsMiddleware{next: h}
	h = &loggingMiddleware{next: h}
	h = &recoveryMiddleware{next: h}

	return h
}

// printBanner prints a startup banner with provider info.
func printBanner(cfg *Config) {
	sep := strings.Repeat("─", 50)
	fmt.Println(sep)
	fmt.Printf("  API Proxy started on %s\n", cfg.Listen)
	fmt.Println(sep)
	fmt.Printf("  Client API keys:  %d configured\n", len(cfg.APIKeys))
	if cfg.Proxy.URL != "" {
		fmt.Printf("  Global proxy:     %s\n", cfg.Proxy.URL)
	}
	if cfg.MaxBodySize > 0 {
		fmt.Printf("  Max body size:    %d bytes\n", cfg.MaxBodySize)
	}
	fmt.Println(sep)
	for _, p := range cfg.Providers {
		proxyInfo := "direct"
		if p.Proxy.URL != "" {
			proxyInfo = p.Proxy.URL
		}
		fmt.Printf("  %-16s %s → %s\n", p.Name, p.PathPrefix, p.Upstream)
		fmt.Printf("  %16s keys=%d timeout=%s retries=%d proxy=%s\n",
			"", len(p.AuthKeys), p.TimeoutDuration, p.MaxRetries, proxyInfo)
	}
	fmt.Println(sep)
}
