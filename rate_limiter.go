package main

import (
	"sync"
	"time"
)

// ClientRateLimiter provides a simple fixed-window rate limiter per API key.
type ClientRateLimiter struct {
	mu     sync.Mutex
	limits map[string]*clientLimit
}

type clientLimit struct {
	count       int
	windowStart time.Time
}

// NewClientRateLimiter creates a new rate limiter.
func NewClientRateLimiter() *ClientRateLimiter {
	return &ClientRateLimiter{
		limits: make(map[string]*clientLimit),
	}
}

// Allow checks if the given API key is allowed to make a request.
// It limits requests to 10 per minute.
func (rl *ClientRateLimiter) Allow(apiKey string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	limit, exists := rl.limits[apiKey]

	if !exists {
		rl.limits[apiKey] = &clientLimit{count: 1, windowStart: now}
		return true
	}

	// If the window has passed (1 minute), reset the counter
	if now.Sub(limit.windowStart) >= time.Minute {
		limit.count = 1
		limit.windowStart = now
		return true
	}

	// Within the same minute window
	if limit.count >= 10 {
		return false
	}

	limit.count++
	return true
}
