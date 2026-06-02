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

func nextDayPTCooldown() time.Time {
	nowPT := time.Now().In(ptLocation)
	return time.Date(nowPT.Year(), nowPT.Month(), nowPT.Day()+1, 0, 1, 0, 0, ptLocation)
}

// keyRateLimit tracks the rate limit for a specific bucket.
type keyRateLimit struct {
	count       int
	windowStart time.Time
}

// KeyManager manages key selection, rate limiting, and cooldown for a single provider.
type KeyManager struct {
	mu              sync.Mutex
	keys            []string
	currentIndex    int
	globalRateLimit int
	cooldownMap     map[int]time.Time        // key index → cooldown expiry time
	rateLimits      map[string]*keyRateLimit // bucketKey → limit state
	consecutive429  map[int]int              // key index → consecutive 429 count
}

// NewKeyManager creates a new KeyManager for an auth provider.
func NewKeyManager(keys []string, globalRateLimit int) *KeyManager {
	return &KeyManager{
		keys:            keys,
		currentIndex:    0,
		globalRateLimit: globalRateLimit,
		cooldownMap:     make(map[int]time.Time),
		rateLimits:      make(map[string]*keyRateLimit),
		consecutive429:  make(map[int]int),
	}
}

// GetKey returns the next available key and its index using round-robin.
// It applies the modelRateLimit if > 0, otherwise falls back to globalRateLimit.
func (km *KeyManager) GetKey(modelID string, modelRateLimit int) (string, int, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	now := time.Now()
	n := len(km.keys)

	// Try starting from currentIndex, scan all keys once
	for i := 0; i < n; i++ {
		idx := (km.currentIndex + i) % n
		if km.isAvailable(idx, now) {
			if km.tryConsumeToken(idx, modelID, modelRateLimit, now) {
				km.currentIndex = (idx + 1) % n
				return km.keys[idx], idx, nil
			}
		}
	}

	return "", -1, fmt.Errorf("all keys are in cooldown or rate limited")
}

// MarkFailed marks a key as failed (400/401), setting its cooldown expiry to next day PT.
func (km *KeyManager) MarkFailed(index int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	km.cooldownMap[index] = nextDayPTCooldown()
	km.consecutive429[index] = 0
}

// Mark429 handles a 429 response with graduated cooldown.
// First/second consecutive 429: 1 minute cooldown.
// Third consecutive 429: next day PT cooldown.
func (km *KeyManager) Mark429(index int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	km.consecutive429[index]++
	count := km.consecutive429[index]

	if count >= 3 {
		km.cooldownMap[index] = nextDayPTCooldown()
		log.Printf("key[%d]: 3 consecutive 429s, cooldown until next day PT", index)
	} else {
		km.cooldownMap[index] = time.Now().Add(1 * time.Minute)
		log.Printf("key[%d]: 429 #%d, cooldown for 1 minute", index, count)
	}
}

// ResetFailures resets the consecutive 429 counter for a key (called on success).
func (km *KeyManager) ResetFailures(index int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if km.consecutive429[index] > 0 {
		km.consecutive429[index] = 0
	}
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

// tryConsumeToken checks if the key at index can be used in its specific bucket.
func (km *KeyManager) tryConsumeToken(index int, modelID string, modelRateLimit int, now time.Time) bool {
	var bucketKey string
	var limitValue int

	if modelRateLimit > 0 {
		bucketKey = fmt.Sprintf("model:%s:%d", modelID, index)
		limitValue = modelRateLimit
	} else {
		bucketKey = fmt.Sprintf("provider:%d", index)
		limitValue = km.globalRateLimit
	}

	limit, exists := km.rateLimits[bucketKey]
	if !exists {
		km.rateLimits[bucketKey] = &keyRateLimit{count: 1, windowStart: now}
		return true
	}

	if now.Sub(limit.windowStart) >= time.Minute {
		limit.count = 1
		limit.windowStart = now
		return true
	}

	if limit.count >= limitValue {
		return false
	}

	limit.count++
	return true
}
