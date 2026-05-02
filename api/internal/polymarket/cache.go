package polymarket

import (
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// TTL profiles per CLAUDE.md "Polymarket API Reference":
//   - 5 min  for market metadata (Gamma /markets, /search, /events)
//   - 30 sec for prices/midpoints
//   - 60 sec for orderbooks
const (
	TTLMetadata  = 5 * time.Minute
	TTLPrice     = 30 * time.Second
	TTLOrderbook = 60 * time.Second

	defaultCacheSize = 1024
)

// ttlEntry pairs a cached payload with its expiry timestamp.
type ttlEntry struct {
	body      []byte
	expiresAt time.Time
}

// ttlCache is a TTL-aware wrapper around a single LRU keyed by URL+params.
// Each entry carries its own expiry; the wrapper checks expiry on read and
// evicts stale entries lazily.
type ttlCache struct {
	mu  sync.Mutex
	lru *lru.Cache[string, ttlEntry]
	now func() time.Time // injectable for tests
}

func newTTLCache(size int) *ttlCache {
	if size <= 0 {
		size = defaultCacheSize
	}
	c, err := lru.New[string, ttlEntry](size)
	if err != nil {
		// lru.New only errors on size <= 0, which we've guarded above.
		panic(err)
	}
	return &ttlCache{lru: c, now: time.Now}
}

// get returns (body, true) if the entry exists AND is not expired.
// Expired entries are removed.
func (c *ttlCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.lru.Get(key)
	if !ok {
		return nil, false
	}
	if c.now().After(e.expiresAt) {
		c.lru.Remove(key)
		return nil, false
	}
	// Defensive copy so the caller can't mutate the cached buffer.
	out := make([]byte, len(e.body))
	copy(out, e.body)
	return out, true
}

// put stores body under key with the given TTL. A zero or negative TTL is a
// no-op (don't cache).
func (c *ttlCache) put(key string, body []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := make([]byte, len(body))
	copy(stored, body)
	c.lru.Add(key, ttlEntry{body: stored, expiresAt: c.now().Add(ttl)})
}
