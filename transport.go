package main

import (
	"context"
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

func streamResponse(w http.ResponseWriter, resp *http.Response) {
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	contentType := resp.Header.Get("Content-Type")
	isStreaming := strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "application/stream") ||
		resp.Header.Get("Transfer-Encoding") == "chunked"

	if isStreaming && canFlush {
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
		io.Copy(w, resp.Body)
	}
}
