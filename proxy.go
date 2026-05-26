package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// hopByHopHeaders are headers that should not be forwarded to the upstream.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// ProviderHandler holds the runtime state for a single provider.
type ProviderHandler struct {
	provider   Provider
	keyManager *KeyManager
	client     *http.Client
}

// NewProviderHandler creates a ProviderHandler with its own http.Client and transport.
func NewProviderHandler(p Provider) *ProviderHandler {
	transport := buildTransport(p.Proxy.URL)
	client := &http.Client{
		Transport: transport,
		Timeout:   0, // We handle timeout per-request via context
		// Don't follow redirects; forward them as-is to the client
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &ProviderHandler{
		provider:   p,
		keyManager: NewKeyManager(p.AuthKeys),
		client:     client,
	}
}

// buildTransport creates an http.Transport with optional proxy support.
func buildTransport(proxyURL string) *http.Transport {
	base := &http.Transport{
		MaxIdleConns:        40,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}

	if proxyURL == "" {
		return base
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		log.Printf("WARN: invalid proxy URL %q, using direct connection: %v", proxyURL, err)
		return base
	}

	switch u.Scheme {
	case "http", "https":
		base.Proxy = http.ProxyURL(u)
	case "socks5":
		dialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			log.Printf("WARN: failed to create SOCKS5 dialer for %q: %v", proxyURL, err)
			return base
		}
		// Use ContextDialer if available for proper context cancellation.
		// The SOCKS5 dialer sends the hostname (not IP) to the proxy server
		// using DOMAINNAME address type (0x03), so DNS is resolved remotely.
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			base.DialContext = cd.DialContext
		} else {
			base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	default:
		log.Printf("WARN: unsupported proxy scheme %q, using direct connection", u.Scheme)
	}

	return base
}

// ServeHTTP handles an incoming request for this provider.
func (ph *ProviderHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip path prefix and build upstream URL
	trimmedPath := strings.TrimPrefix(r.URL.Path, ph.provider.PathPrefix)
	if trimmedPath == "" {
		trimmedPath = "/"
	}

	upstreamURL := ph.provider.Upstream + trimmedPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Buffer request body for potential retries
	var bodyBytes []byte
	var err error
	if r.Body != nil {
		bodyBytes, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
	}

	maxAttempts := ph.provider.MaxRetries + 1 // first attempt + retries
	var lastStatusCode int

	for attempt := 0; attempt < maxAttempts; attempt++ {
		key, keyIndex, err := ph.keyManager.GetKey()
		if err != nil {
			// All keys in cooldown
			log.Printf("provider=%s: all keys in cooldown, returning 503", ph.provider.Name)
			http.Error(w, "Service Unavailable: all upstream keys are in cooldown", http.StatusServiceUnavailable)
			return
		}

		statusCode, done := ph.forwardRequest(w, r, upstreamURL, key, keyIndex, bodyBytes)
		lastStatusCode = statusCode

		if done {
			return
		}

		// If we reach here, forwardRequest indicated a retryable failure (401/403/429)
		log.Printf("provider=%s: key[%d] failed with status %d, attempt %d/%d",
			ph.provider.Name, keyIndex, statusCode, attempt+1, maxAttempts)
	}

	// All retries exhausted
	log.Printf("provider=%s: all retries exhausted (last status: %d), returning 503",
		ph.provider.Name, lastStatusCode)
	http.Error(w, "Service Unavailable: all upstream keys failed", http.StatusServiceUnavailable)
}

// forwardRequest sends a single request to the upstream.
// Returns (statusCode, done). If done is true, the response has been written to w.
// If done is false, the caller should retry with a different key.
func (ph *ProviderHandler) forwardRequest(
	w http.ResponseWriter,
	originalReq *http.Request,
	upstreamURL string,
	authKey string,
	keyIndex int,
	bodyBytes []byte,
) (int, bool) {
	// Create request with timeout context
	ctx, cancel := context.WithTimeout(originalReq.Context(), ph.provider.TimeoutDuration)
	defer cancel()

	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = strings.NewReader(string(bodyBytes))
	}

	upstreamReq, err := http.NewRequestWithContext(ctx, originalReq.Method, upstreamURL, bodyReader)
	if err != nil {
		log.Printf("provider=%s: failed to create upstream request: %v", ph.provider.Name, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return 0, true
	}

	// Copy headers from original request, skip hop-by-hop headers
	copyHeaders(upstreamReq.Header, originalReq.Header)

	// Set the upstream auth key
	upstreamReq.Header.Set(ph.provider.AuthHeader, authKey)

	// Send request to upstream
	resp, err := ph.client.Do(upstreamReq)
	if err != nil {
		log.Printf("provider=%s: upstream request error: %v", ph.provider.Name, err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return 0, true
	}
	defer resp.Body.Close()

	// Check for retryable status codes
	if isKeyFailureStatus(resp.StatusCode) || resp.StatusCode == http.StatusServiceUnavailable {
		// Drain the response body before retrying
		io.Copy(io.Discard, resp.Body)
		if isKeyFailureStatus(resp.StatusCode) {
			ph.keyManager.MarkFailed(keyIndex)
		}
		return resp.StatusCode, false // Signal caller to retry
	}

	// Stream the response back to the client
	streamResponse(w, resp)
	return resp.StatusCode, true
}

// isKeyFailureStatus checks if the HTTP status code indicates the auth key is invalid/exhausted.
func isKeyFailureStatus(code int) bool {
	return code == http.StatusBadRequest || // 400
		code == http.StatusTooManyRequests // 429
}

// copyHeaders copies headers from src to dst, excluding hop-by-hop headers.
func copyHeaders(dst, src http.Header) {
	hopSet := make(map[string]bool, len(hopByHopHeaders))
	for _, h := range hopByHopHeaders {
		hopSet[strings.ToLower(h)] = true
	}

	for key, values := range src {
		if hopSet[strings.ToLower(key)] {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// streamResponse streams the upstream response back to the client.
func streamResponse(w http.ResponseWriter, resp *http.Response) {
	// Copy response headers
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	// Check if we should flush for streaming responses (SSE, etc.)
	flusher, canFlush := w.(http.Flusher)
	contentType := resp.Header.Get("Content-Type")
	isStreaming := strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "application/stream") ||
		resp.Header.Get("Transfer-Encoding") == "chunked"

	if isStreaming && canFlush {
		// Stream with flushing for real-time responses
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				flusher.Flush()
			}
			if err != nil {
				break
			}
		}
	} else {
		// Regular copy for non-streaming responses
		io.Copy(w, resp.Body)
	}
}

// ProxyRouter holds all provider handlers and routes requests.
type ProxyRouter struct {
	handlers []*ProviderHandler
	apiKeys  map[string]bool
	limiter  *ClientRateLimiter
}

// NewProxyRouter creates a ProxyRouter from the config.
func NewProxyRouter(cfg *Config) *ProxyRouter {
	handlers := make([]*ProviderHandler, len(cfg.Providers))
	for i, p := range cfg.Providers {
		handlers[i] = NewProviderHandler(p)
		log.Printf("Registered provider %q: %s -> %s", p.Name, p.PathPrefix, p.Upstream)
	}

	apiKeys := make(map[string]bool, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		apiKeys[k] = true
	}

	return &ProxyRouter{
		handlers: handlers,
		apiKeys:  apiKeys,
		limiter:  NewClientRateLimiter(),
	}
}

// ServeHTTP routes incoming requests to the appropriate provider handler.
func (pr *ProxyRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health check endpoint
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
		return
	}

	// Find matching provider by path prefix
	for _, handler := range pr.handlers {
		if strings.HasPrefix(r.URL.Path, handler.provider.PathPrefix) {
			// Authenticate the client
			authValue := r.Header.Get(handler.provider.AuthHeader)

			tokenValue := authValue
			if strings.HasPrefix(strings.ToLower(tokenValue), "bearer ") {
				tokenValue = strings.TrimSpace(tokenValue[7:])
			}

			if tokenValue == "" || !pr.apiKeys[tokenValue] {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Rate limit check
			if !pr.limiter.Allow(tokenValue) {
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}

			handler.ServeHTTP(w, r)
			return
		}
	}

	http.Error(w, "Not Found", http.StatusNotFound)
}
