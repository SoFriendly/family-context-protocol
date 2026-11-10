package server

import (
	"sync"
	"time"

	"github.com/sofriendly/family-context-protocol/internal/store"
)

// Limiter is an in-memory token bucket per key. Limits are per minute.
// ponytail: buckets are never evicted; fine for a household's handful of keys
// and IPs, add a sweep if a server ever faces many distinct callers.
type Limiter struct {
	mu sync.Mutex
	b  map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewLimiter() *Limiter { return &Limiter{b: map[string]*bucket{}} }

// Allow applies the per-key read (60/min) or write (30/min) limit.
func (l *Limiter) Allow(key string, write bool) bool {
	if write {
		return l.AllowN("w:"+store.Hash(key), 30)
	}
	return l.AllowN("r:"+store.Hash(key), 60)
}

// AllowN takes one token from the named bucket refilling at perMin/minute.
func (l *Limiter) AllowN(name string, perMin int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.b[name]
	if !ok {
		b = &bucket{tokens: float64(perMin), last: now}
		l.b[name] = b
	}
	b.tokens = min(float64(perMin), b.tokens+now.Sub(b.last).Minutes()*float64(perMin))
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
