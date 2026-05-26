package main

import (
	"fmt"
	"log"
	"sync"
	"time"
	_ "time/tzdata"
)

var ptLocation *time.Location

func init() {
	var err error
	ptLocation, err = time.LoadLocation("America/Los_Angeles")
	if err != nil {
		log.Printf("WARN: failed to load America/Los_Angeles timezone: %v", err)
		ptLocation = time.UTC
	}
}

// keyRateLimit tracks the rate limit for a single upstream key.
type keyRateLimit struct {
	count       int
	windowStart time.Time
}

// KeyManager manages key selection and cooldown for a single provider.
type KeyManager struct {
	mu           sync.Mutex
	keys         []string
	currentIndex int
	cooldownMap  map[int]time.Time // key index → cooldown expiry time
	rateLimits   map[int]*keyRateLimit
}

// NewKeyManager creates a new KeyManager with the given keys.
func NewKeyManager(keys []string) *KeyManager {
	return &KeyManager{
		keys:         keys,
		currentIndex: 0,
		cooldownMap:  make(map[int]time.Time),
		rateLimits:   make(map[int]*keyRateLimit),
	}
}

// GetKey returns the next available key and its index using round-robin.
// It skips keys that are in cooldown or rate limited.
// Returns an error if all keys are in cooldown or rate limited.
func (km *KeyManager) GetKey() (string, int, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	now := time.Now()
	n := len(km.keys)

	// Try starting from currentIndex, scan all keys once
	for i := 0; i < n; i++ {
		idx := (km.currentIndex + i) % n
		if km.isAvailable(idx, now) {
			if km.tryConsumeToken(idx, now) {
				km.currentIndex = (idx + 1) % n
				return km.keys[idx], idx, nil
			}
		}
	}

	return "", -1, fmt.Errorf("all keys are in cooldown or rate limited")
}

// MarkFailed marks a key as failed, setting its cooldown expiry.
func (km *KeyManager) MarkFailed(index int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	nowPT := time.Now().In(ptLocation)
	nextDayPT := time.Date(nowPT.Year(), nowPT.Month(), nowPT.Day()+1, 0, 1, 0, 0, ptLocation)
	km.cooldownMap[index] = nextDayPT
}

// isAvailable checks if a key is not in cooldown (or cooldown has expired).
func (km *KeyManager) isAvailable(index int, now time.Time) bool {
	expiry, exists := km.cooldownMap[index]
	if !exists {
		return true
	}
	if now.After(expiry) {
		// Cooldown expired, clean up
		delete(km.cooldownMap, index)
		return true
	}
	return false
}

// tryConsumeToken checks if the key at index can be used (max 5 times per minute).
// If allowed, it increments the usage count and returns true.
func (km *KeyManager) tryConsumeToken(index int, now time.Time) bool {
	limit, exists := km.rateLimits[index]
	if !exists {
		km.rateLimits[index] = &keyRateLimit{count: 1, windowStart: now}
		return true
	}

	if now.Sub(limit.windowStart) >= time.Minute {
		limit.count = 1
		limit.windowStart = now
		return true
	}

	if limit.count >= 5 {
		return false
	}

	limit.count++
	return true
}
